/*
Copyright 2018 The Kubernetes Authors.

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

package docker

import (
	"context"
	"fmt"
	"net"

	"github.com/pkg/errors"
	"sigs.k8s.io/cluster-api/test/infrastructure/docker/docker/types"
	"sigs.k8s.io/cluster-api/test/infrastructure/docker/third_party/forked/loadbalancer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/kind/pkg/cluster/constants"
)

type lbCreator interface {
	CreateExternalLoadBalancerNode(name, image, clusterLabel, listenAddress string, port int32) (*types.Node, error)
}

// LoadBalancer manages the load balancer for a specific docker cluster.
type LoadBalancer struct {
	name      string
	container *types.Node
	ipv6      bool

	lbCreator lbCreator
}

// NewLoadBalancer returns a new helper for managing a docker loadbalancer with a given name.
func NewLoadBalancer(name string, ipv6 bool) (*LoadBalancer, error) {
	if name == "" {
		return nil, errors.New("name is required when creating a docker.LoadBalancer")
	}

	// look for the container that is hosting the loadbalancer for the cluster.
	// filter based on the label and the roles regardless of whether or not it is running.
	// if non-running container is chosen, then it will not have an IP address associated with it.
	container, err := getContainer(
		withLabel(clusterLabel(name)),
		withLabel(roleLabel(constants.ExternalLoadBalancerNodeRoleValue)),
	)
	if err != nil {
		return nil, err
	}

	return &LoadBalancer{
		name:      name,
		container: container,
		ipv6:      ipv6,
		lbCreator: &Manager{},
	}, nil
}

// ContainerName is the name of the docker container with the load balancer.
func (s *LoadBalancer) containerName() string {
	return fmt.Sprintf("%s-lb", s.name)
}

// Create creates a docker container hosting a load balancer for the cluster.
func (s *LoadBalancer) Create(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx)
	log = log.WithValues("cluster name", s.name, "ipv6", s.ipv6)

	listenAddr := "0.0.0.0"
	if s.ipv6 {
		listenAddr = "::"
	}
	// Create if not exists.
	if s.container == nil {
		var err error
		log.Info("Creating load balancer container")
		s.container, err = s.lbCreator.CreateExternalLoadBalancerNode(
			s.containerName(),
			loadbalancer.Image,
			clusterLabel(s.name),
			listenAddr,
			0,
		)
		if err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// UpdateConfiguration updates the external load balancer configuration with new control plane nodes.
func (s *LoadBalancer) UpdateConfiguration(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx)

	if s.container == nil {
		return errors.New("unable to configure load balancer: load balancer container does not exists")
	}

	// collect info about the existing controlplane nodes
	controlPlaneNodes, err := listContainers(
		withLabel(clusterLabel(s.name)),
		withLabel(roleLabel(constants.ControlPlaneNodeRoleValue)),
	)
	if err != nil {
		return errors.WithStack(err)
	}

	var backendServers = map[string]string{}
	for _, n := range controlPlaneNodes {
		controlPlaneIP, controlPlaneIPv6, err := n.IP(ctx)
		if err != nil {
			return errors.Wrapf(err, "failed to get IP for container %s", n.String())
		}
		if s.ipv6 {
			controlPlaneIP = controlPlaneIPv6
		}
		backendServers[n.String()] = net.JoinHostPort(controlPlaneIP, "6443")
	}

	loadBalancerConfig, err := loadbalancer.Config(&loadbalancer.ConfigData{
		ControlPlanePort: 6443,
		BackendServers:   backendServers,
		IPv6:             s.ipv6,
	})
	if err != nil {
		return errors.WithStack(err)
	}

	log.Info("Updating load balancer configuration")
	if err := s.container.WriteFile(ctx, loadbalancer.ConfigPath, loadBalancerConfig); err != nil {
		return errors.WithStack(err)
	}

	return errors.WithStack(s.container.Kill(ctx, "SIGHUP"))
}

// IP returns the load balancer IP address.
func (s *LoadBalancer) IP(ctx context.Context) (string, error) {
	lbip, lbip6, err := s.container.IP(ctx)
	if err != nil {
		return "", errors.WithStack(err)
	}
	if s.ipv6 {
		lbip = lbip6
	}
	if lbip == "" {
		// if there is a load balancer container with the same name exists but is stopped, it may not have IP address associated with it.
		return "", errors.Errorf("load balancer IP cannot be empty: container %s does not have an associated IP address", s.containerName())
	}
	return lbip, nil
}

// Delete the docker container hosting the cluster load balancer.
func (s *LoadBalancer) Delete(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx)

	if s.container != nil {
		log.Info("Deleting load balancer container")
		if err := s.container.Delete(ctx); err != nil {
			return err
		}
		s.container = nil
	}
	return nil
}
