// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2023, Unikraft GmbH and The KraftKit Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.

// Package compose provides primitives for running Unikraft applications
// via the Compose specification.
package compose

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"

	"kraftkit.sh/log"
	"kraftkit.sh/machine/network/iputils"
	mplatform "kraftkit.sh/machine/platform"
	ukarch "kraftkit.sh/unikraft/arch"
)

type Project struct {
	*types.Project `json:"project"` // The underlying compose-go project
}

// DefaultFileNames is a list of default compose file names to look for
var DefaultFileNames = []string{
	"docker-compose.yml",
	"docker-compose.yaml",
	"compose.yml",
	"compose.yaml",
	"Composefile",
}

// NewProjectFromComposeFile loads a compose file and returns a project. If no
// compose file is specified, it will look for one in the current directory.
func NewProjectFromComposeFile(ctx context.Context, workdir, composefile string, opts ...cli.ProjectOptionsFn) (*Project, error) {
	if composefile == "" {
		for _, file := range DefaultFileNames {
			fullpath := filepath.Join(workdir, file)
			if _, err := os.Stat(fullpath); err == nil {
				log.G(ctx).
					WithField("composefile", fullpath).
					Debugf("using")
				composefile = file
				break
			}
		}
	}

	if composefile == "" {
		return nil, fmt.Errorf("no compose file found")
	}

	fullpath := filepath.Join(workdir, composefile)

	options, err := cli.NewProjectOptions(
		[]string{fullpath},
		opts...,
	)
	if err != nil {
		return nil, err
	}

	project, err := cli.ProjectFromOptions(ctx, options)
	if err != nil {
		return nil, err
	}

	project = project.WithoutUnnecessaryResources()

	project.ComposeFiles = []string{composefile}
	project.WorkingDir = workdir

	return &Project{project}, err
}

// Validate performs some early checks on the project to ensure it is valid,
// as well as fill in some unspecified fields.
func (project *Project) Validate(ctx context.Context) error {
	var err error
	// Check that each service has at least an image name or a build context
	for _, service := range project.Services {
		if service.Image == "" && service.Build == nil {
			return fmt.Errorf("service %s has neither an image nor a build context", service.Name)
		}
	}

	// If the project has no name, use the directory name
	if project.Name == "" {
		// Take the last part of the working directory
		parts := strings.Split(project.WorkingDir, "/")
		project.Name = parts[len(parts)-1]
	}

	project.Project, err = project.WithServicesTransform(func(name string, service types.ServiceConfig) (types.ServiceConfig, error) {
		service.Name = name
		if service.ContainerName == "" {
			service.ContainerName = fmt.Sprint(project.Name, "-", name)
		}
		if service.Platform == "" {
			hostPlatform, _, err := mplatform.Detect(ctx)
			if err != nil {
				return service, err
			}

			hostArch, err := ukarch.HostArchitecture()
			if err != nil {
				return service, err
			}

			service.Platform = fmt.Sprint(hostPlatform, "/", hostArch)
		}

		return service, nil
	},
	)
	if err != nil {
		return err
	}

	return nil
}

func (project *Project) AssignIPs(ctx context.Context) error {
	var err error
	usedAddresses := make(map[string]map[string]struct{})
	for i, network := range project.Networks {
		if network.External || len(network.Ipam.Config) == 0 {
			continue
		}

		// Join all the IPAM configs together
		ipamConfig := network.Ipam.Config[0]
		for _, config := range network.Ipam.Config[1:] {
			if config.Subnet != "" {
				ipamConfig.Subnet = config.Subnet
			}
			if config.Gateway != "" {
				ipamConfig.Gateway = config.Gateway
			}
		}

		if ipamConfig.Subnet == "" {
			return fmt.Errorf("network %s has no subnet specified", network.Name)
		}

		// Check that the subnet is of type addr/subnet
		if len(strings.Split(ipamConfig.Subnet, "/")) != 2 {
			return fmt.Errorf("network %s has an invalid subnet specified", network.Name)
		}

		subnetIP, subnetMask, err := net.ParseCIDR(ipamConfig.Subnet)
		if err != nil {
			return fmt.Errorf("failed to parse %s network subnet", network.Name)
		}

		if subnetMask == nil {
			return fmt.Errorf("failed to parse network %s subnet mask", network.Name)
		}

		// Check that the gateway is of type addr
		if ipamConfig.Gateway == "" {
			ipamConfig.Gateway = subnetIP.String()
		} else {
			// Additionally check the gateway is part of the subnet
			gatewayIP := net.ParseIP(ipamConfig.Gateway)
			if gatewayIP == nil {
				return fmt.Errorf("failed to parse %s network gateway", network.Name)
			}

			if !subnetMask.Contains(gatewayIP) {
				return fmt.Errorf("network %s gateway is not within the subnet", network.Name)
			}
		}

		usedAddresses[i] = make(map[string]struct{})
		usedAddresses[i][ipamConfig.Gateway] = struct{}{}
		usedAddresses[i][subnetMask.IP.String()] = struct{}{}

		network.Ipam.Config[0] = ipamConfig
		project.Networks[i] = network
	}

	// Mark used IPs for services with static IPs
	for _, service := range project.Services {
		if service.Networks == nil {
			continue
		}

		for name, network := range service.Networks {
			if _, ok := project.Networks[name]; !ok {
				return fmt.Errorf("service %s references non-existent network %s", service.Name, name)
			}

			if network != nil && network.Ipv4Address != "" {
				if len(project.Networks[name].Ipam.Config) == 0 {
					return fmt.Errorf("cannot assign IP address to service %s on network %s without IPAM config", service.Name, name)
				}

				usedAddresses[name][network.Ipv4Address] = struct{}{}
			}
		}
	}

	// WithServicesTransform runs in parallel and hence we need to protect the
	// usedAddresses map
	var mu sync.Mutex
	project.Project, err = project.WithServicesTransform(func(name string, service types.ServiceConfig) (types.ServiceConfig, error) {
		if service.Networks == nil {
			return service, nil
		}
		for name, network := range service.Networks {
			if network == nil {
				service.Networks[name] = &types.ServiceNetworkConfig{}
				network = service.Networks[name]
			}

			if network.Ipv4Address != "" || len(project.Networks[name].Ipam.Config) == 0 {
				continue
			}

			// Start at the network's subnet IP and increment until we find
			// a free one
			_, subnet, err := net.ParseCIDR(project.Networks[name].Ipam.Config[0].Subnet)
			if err != nil {
				return service, err
			}

			if subnet == nil {
				// This should not be possible
				return service, fmt.Errorf("failed to parse network %s subnet", name)
			}

			ip := subnet.IP

			mu.Lock()
			for _, exists := usedAddresses[name][ip.String()]; subnet.Contains(ip) && exists; _, exists = usedAddresses[name][ip.String()] {
				ip = iputils.IncreaseIP(ip)
			}

			if !subnet.Contains(ip) {
				mu.Unlock()
				return service, fmt.Errorf("not enough free IP addresses in network %s", name)
			}

			service.Networks[name].Ipv4Address = ip.String()
			usedAddresses[name][ip.String()] = struct{}{}

			// We have to unlock after we marked the ip as used in the map
			mu.Unlock()
		}

		return service, nil
	})
	if err != nil {
		return err
	}

	return nil
}

// ServicesOrderedByDependencies receives a list of services and generates a
// new list ordered by dependencies. If expand is set, it will also include
// dependencies not present in the original list.
func (project *Project) ServicesOrderedByDependencies(ctx context.Context, services types.Services, expand bool) []types.ServiceConfig {
	added := map[string]struct{}{}

	var addDependenciesRecursevly func(service types.ServiceConfig)

	orderedServices := []types.ServiceConfig{}
	addDependenciesRecursevly = func(service types.ServiceConfig) {
		added[service.Name] = struct{}{}
		for name, dependency := range service.DependsOn {
			_, ok := services[name]
			if !ok && !expand {
				continue
			}

			log.G(ctx).
				WithField("service", service.Name).
				WithField("on", name).
				Debug("depends")

			_, ok = added[name]
			if !ok && dependency.Required {
				addDependenciesRecursevly(project.Services[name])
			}
		}

		orderedServices = append(orderedServices, service)
	}

	for _, service := range services {
		_, ok := added[service.Name]
		if !ok {
			addDependenciesRecursevly(service)
		}
	}

	return orderedServices
}

// ServicesReversedWithDependants receives a list of services and generates a
// new list reverse ordered by dependencies. If expand is set, it will also
// include dependendants not present in the original list.
func (project *Project) ServicesReversedByDependencies(ctx context.Context, services types.Services, expand bool) []types.ServiceConfig {
	added := map[string]struct{}{}

	var addDependantsRecursevly func(service types.ServiceConfig)

	reversedServices := []types.ServiceConfig{}
	addDependantsRecursevly = func(service types.ServiceConfig) {
		added[service.Name] = struct{}{}
		for _, name := range service.GetDependents(project.Project) {
			_, ok := services[name]
			if !ok && !expand {
				continue
			}

			log.G(ctx).
				WithField("service", name).
				WithField("on", service.Name).
				Debug("depends")

			_, ok = added[name]
			if !ok && project.Services[name].DependsOn[service.Name].Required {
				addDependantsRecursevly(project.Services[name])
			}
		}

		reversedServices = append(reversedServices, service)
	}

	for _, service := range services {
		_, ok := added[service.Name]
		if !ok {
			addDependantsRecursevly(service)
		}
	}

	return reversedServices
}
