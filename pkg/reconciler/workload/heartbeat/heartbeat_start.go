/*
Copyright 2022 The KCP Authors.

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

package heartbeat

import (
	"fmt"
	"time"

	"github.com/spf13/pflag"
)

func DefaultOptions() *Options {
	return &Options{
		HeartbeatThreshold: time.Minute,
	}
}

func BindOptions(o *Options, fs *pflag.FlagSet) *Options {
	fs.DurationVar(&o.HeartbeatThreshold, "workload-cluster-heartbeat-threshold", o.HeartbeatThreshold, "Amount of time to wait for a successful heartbeat before marking the cluster as not ready")
	return o
}

type Options struct {
	HeartbeatThreshold time.Duration
}

func (o *Options) Validate() error {
	if o.HeartbeatThreshold <= 0 {
		return fmt.Errorf("--workload-cluster-heartbeat-threshold must be >0 (%s)", o.HeartbeatThreshold)
	}
	return nil
}
