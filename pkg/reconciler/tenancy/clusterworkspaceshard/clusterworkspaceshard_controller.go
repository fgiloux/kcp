/*
Copyright 2021 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package clusterworkspaceshard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	kcpcache "github.com/kcp-dev/apimachinery/pkg/cache"
	"github.com/kcp-dev/logicalcluster/v2"

	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	tenancyv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/tenancy/v1alpha1"
	kcpclient "github.com/kcp-dev/kcp/pkg/client/clientset/versioned"
	tenancyinformers "github.com/kcp-dev/kcp/pkg/client/informers/externalversions/tenancy/v1alpha1"
	tenancylisters "github.com/kcp-dev/kcp/pkg/client/listers/tenancy/v1alpha1"
	"github.com/kcp-dev/kcp/pkg/logging"
)

const (
	ControllerName = "kcp-clusterworkspaceshard"
)

func NewController(
	rootKcpClient kcpclient.Interface,
	clusterWorkspaceShardInformer tenancyinformers.ClusterWorkspaceShardInformer,
) (*Controller, error) {
	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), ControllerName)

	c := &Controller{
		queue:                        queue,
		kcpClient:                    rootKcpClient,
		clusterWorkspaceShardIndexer: clusterWorkspaceShardInformer.Informer().GetIndexer(),
		clusterWorkspaceShardLister:  clusterWorkspaceShardInformer.Lister(),
	}

	clusterWorkspaceShardInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueue(obj) },
		UpdateFunc: func(_, obj interface{}) { c.enqueue(obj) },
	})

	return c, nil
}

// Controller watches WorkspaceShards and Secrets in order to make sure every ClusterWorkspaceShard
// has its URL exposed when a valid kubeconfig is connected to it.
type Controller struct {
	queue workqueue.RateLimitingInterface

	kcpClient kcpclient.Interface

	clusterWorkspaceShardIndexer cache.Indexer
	clusterWorkspaceShardLister  tenancylisters.ClusterWorkspaceShardLister
}

func (c *Controller) enqueue(obj interface{}) {
	key, err := kcpcache.MetaClusterNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	logger := logging.WithQueueKey(logging.WithReconciler(klog.Background(), ControllerName), key)
	logger.V(2).Info("queueing ClusterWorkspaceShard")
	c.queue.Add(key)
}

func (c *Controller) Start(ctx context.Context, numThreads int) {
	defer runtime.HandleCrash()
	defer c.queue.ShutDown()

	logger := logging.WithReconciler(klog.FromContext(ctx), ControllerName)
	ctx = klog.NewContext(ctx, logger)
	logger.Info("Starting controller")
	defer logger.Info("Shutting down controller")

	for i := 0; i < numThreads; i++ {
		go wait.Until(func() { c.startWorker(ctx) }, time.Second, ctx.Done())
	}

	<-ctx.Done()
}

func (c *Controller) startWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	// Wait until there is a new item in the working queue
	k, quit := c.queue.Get()
	if quit {
		return false
	}
	key := k.(string)

	logger := logging.WithQueueKey(klog.FromContext(ctx), key)
	ctx = klog.NewContext(ctx, logger)
	logger.V(1).Info("processing key")

	// No matter what, tell the queue we're done with this key, to unblock
	// other workers.
	defer c.queue.Done(key)

	if requeue, err := c.process(ctx, key); err != nil {
		runtime.HandleError(fmt.Errorf("%q controller failed to sync %q, err: %w", ControllerName, key, err))
		c.queue.AddRateLimited(key)
		return true
	} else if requeue {
		// only requeue if we didn't error, but we still want to requeue
		c.queue.Add(key)
	}
	c.queue.Forget(key)
	return true
}

func (c *Controller) process(ctx context.Context, key string) (bool, error) {
	logger := klog.FromContext(ctx)
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logger.Error(err, "invalid key")
		return false, nil
	}
	if namespace != "" {
		logger.Error(errors.New("namespace found in key for cluster-wide ClusterWorkspaceShard object"), "invalid key")
		return false, nil
	}

	obj, err := c.clusterWorkspaceShardLister.Get(key)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return false, nil // object deleted before we handled it
		}
		return true, err
	}
	previous := obj
	obj = obj.DeepCopy()

	logger = logging.WithObject(logger, obj)
	ctx = klog.NewContext(ctx, logger)

	if err := c.reconcile(ctx, obj); err != nil {
		return true, err
	}

	// If the status of the object being reconciled changed as a result, update it.
	if !equality.Semantic.DeepEqual(previous.Status, obj.Status) {
		oldData, err := json.Marshal(tenancyv1alpha1.ClusterWorkspaceShard{
			ObjectMeta: metav1.ObjectMeta{
				UID:             previous.UID,
				ResourceVersion: previous.ResourceVersion,
			},
			Status: previous.Status,
		})
		if err != nil {
			return true, fmt.Errorf("failed to Marshal old data for workspace shard %s|%s/%s: %w", tenancyv1alpha1.RootCluster, namespace, name, err)
		}

		newData, err := json.Marshal(tenancyv1alpha1.ClusterWorkspaceShard{
			ObjectMeta: metav1.ObjectMeta{
				UID:             previous.UID,
				ResourceVersion: previous.ResourceVersion,
			}, // to ensure they appear in the patch as preconditions
			Status: obj.Status,
		})
		if err != nil {
			return true, fmt.Errorf("failed to Marshal new data for workspace shard %s|%s/%s: %w", tenancyv1alpha1.RootCluster, namespace, name, err)
		}

		patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
		if err != nil {
			return true, fmt.Errorf("failed to create patch for workspace shard %s|%s/%s: %w", tenancyv1alpha1.RootCluster, namespace, name, err)
		}
		_, uerr := c.kcpClient.TenancyV1alpha1().ClusterWorkspaceShards().Patch(logicalcluster.WithCluster(ctx, tenancyv1alpha1.RootCluster), obj.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status")
		if uerr != nil {
			return true, uerr
		}

		return true, nil
	}

	// If the labels of the object being reconciled changed as a result, update it.
	if previous.Labels == nil ||
		!equality.Semantic.DeepEqual(previous.Labels, obj.Labels) {
		oldData, err := json.Marshal(tenancyv1alpha1.ClusterWorkspaceShard{
			ObjectMeta: metav1.ObjectMeta{
				UID:             previous.UID,
				ResourceVersion: previous.ResourceVersion,
				Labels:          previous.Labels,
			},
		})
		if err != nil {
			return true, fmt.Errorf("failed to Marshal old data for workspace shard %s|%s/%s: %w", tenancyv1alpha1.RootCluster, namespace, name, err)
		}

		newData, err := json.Marshal(tenancyv1alpha1.ClusterWorkspaceShard{
			ObjectMeta: metav1.ObjectMeta{
				UID:             previous.UID,
				ResourceVersion: previous.ResourceVersion,
				Labels:          obj.Labels,
			}, // to ensure they appear in the patch as preconditions
		})
		if err != nil {
			return true, fmt.Errorf("failed to Marshal new data for workspace shard %s|%s/%s: %w", tenancyv1alpha1.RootCluster, namespace, name, err)
		}

		patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
		if err != nil {
			return true, fmt.Errorf("failed to create patch for workspace shard %s|%s/%s: %w", tenancyv1alpha1.RootCluster, namespace, name, err)
		}
		_, uerr := c.kcpClient.TenancyV1alpha1().ClusterWorkspaceShards().Patch(logicalcluster.WithCluster(ctx, tenancyv1alpha1.RootCluster), obj.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
		if uerr != nil {
			return true, uerr
		}

		return true, nil
	}

	logger.V(6).Info("processed ClusterWorkspaceShard")
	return false, nil
}

func (c *Controller) reconcile(ctx context.Context, workspaceShard *tenancyv1alpha1.ClusterWorkspaceShard) error {
	if workspaceShard.Labels == nil {
		workspaceShard.Labels = map[string]string{}
	}
	workspaceShard.Labels["tenancy.kcp.dev/shard"] = workspaceShard.Name
	return nil
}
