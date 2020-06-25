// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"os"
	"reflect"

	"go.universe.tf/metallb/internal/k8s"
	"go.universe.tf/metallb/internal/logging"
	"go.universe.tf/metallb/internal/version"

	"github.com/go-kit/kit/log"
	"k8s.io/api/core/v1"
)

// Service offers methods to mutate a Kubernetes service object.
type service interface {
	UpdateStatus(svc *v1.Service) error
	Infof(svc *v1.Service, desc, msg string, args ...interface{})
	Errorf(svc *v1.Service, desc, msg string, args ...interface{})
}

type controller struct {
	client service
}

func (c *controller) SetBalancer(l log.Logger, name string, svcRo *v1.Service, _ *v1.Endpoints) k8s.SyncState {
	l.Log("event", "startUpdate", "msg", "start of service update")
	defer l.Log("event", "endUpdate", "msg", "end of service update")

	if svcRo == nil {
		return k8s.SyncStateSuccess
	}

	svc := svcRo.DeepCopy()

	// At this point, we have an IP selected somehow, all that remains
	// is to program the data plane.
	svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: svc.Spec.LoadBalancerIP}}

	if !reflect.DeepEqual(svcRo.Status, svc.Status) {
		if err := c.client.UpdateStatus(svc); err != nil {
			l.Log("op", "updateServiceStatus", "error", err, "msg", "failed to update service status")
			return k8s.SyncStateError
		}
	}
	l.Log("event", "serviceUpdated", "msg", "updated service object")

	return k8s.SyncStateSuccess
}

func main() {
	logger, err := logging.Init()
	if err != nil {
		fmt.Printf("failed to initialize logging: %s\n", err)
		os.Exit(1)
	}

	var (
		port   = flag.Int("port", 7472, "HTTP listening port for Prometheus metrics")
	)
	flag.Parse()

	logger.Log("version", version.Version(), "commit", version.CommitHash(), "branch", version.Branch(), "msg", "MetalLB controller starting "+version.String())

	c := &controller{}

	client, err := k8s.New(&k8s.Config{
		ProcessName:   "metallb-controller",
		MetricsPort:   *port,
		Logger:        logger,

		ServiceChanged: c.SetBalancer,
	})
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create k8s client")
		os.Exit(1)
	}

	c.client = client
	if err := client.Run(nil); err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to run k8s client")
	}
}
