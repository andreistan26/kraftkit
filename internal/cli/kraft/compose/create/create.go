// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2024, Unikraft GmbH and The KraftKit Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.

package create

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/MakeNowJust/heredoc"
	composespec "github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/spf13/cobra"

	"kraftkit.sh/cmdfactory"
	"kraftkit.sh/compose"
	"kraftkit.sh/internal/cli/kraft/build"
	"kraftkit.sh/internal/cli/kraft/compose/utils"
	netcreate "kraftkit.sh/internal/cli/kraft/net/create"
	"kraftkit.sh/internal/cli/kraft/pkg"
	"kraftkit.sh/internal/cli/kraft/pkg/pull"
	"kraftkit.sh/internal/cli/kraft/remove"
	"kraftkit.sh/internal/cli/kraft/run"
	volcreate "kraftkit.sh/internal/cli/kraft/volume/create"
	"kraftkit.sh/log"
	"kraftkit.sh/packmanager"
	"kraftkit.sh/unikraft"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	composeapi "kraftkit.sh/api/compose/v1"
	machineapi "kraftkit.sh/api/machine/v1alpha1"
	networkapi "kraftkit.sh/api/network/v1alpha1"
	volumeapi "kraftkit.sh/api/volume/v1alpha1"
	mnetwork "kraftkit.sh/machine/network"
	mplatform "kraftkit.sh/machine/platform"
	mvolume "kraftkit.sh/machine/volume"
	"kraftkit.sh/unikraft/export/v0/uknetdev"
)

type CreateOptions struct {
	Composefile   string `noattribute:"true"`
	EnvFile       string `noattribute:"true"`
	RemoveOrphans bool   `long:"remove-orphans" usage:"Remove machines for services not defined in the Compose file"`
}

func NewCmd() *cobra.Command {
	cmd, err := cmdfactory.New(&CreateOptions{}, cobra.Command{
		Short:   "Create a compose project",
		Use:     "create [FLAGS]",
		Aliases: []string{},
		Long:    "Create the services and networks for a project.",
		Example: heredoc.Doc(`
			# Create the networks and services without running them
			$ kraft compose create 
		`),
		Annotations: map[string]string{
			cmdfactory.AnnotationHelpGroup: "compose",
		},
	})
	if err != nil {
		panic(err)
	}

	return cmd
}

func (opts *CreateOptions) Pre(cmd *cobra.Command, _ []string) error {
	ctx, err := packmanager.WithDefaultUmbrellaManagerInContext(cmd.Context())
	if err != nil {
		return err
	}

	cmd.SetContext(ctx)

	if cmd.Flag("file").Changed {
		opts.Composefile = cmd.Flag("file").Value.String()
	}

	log.G(cmd.Context()).WithField("composefile", opts.Composefile).Debug("using")
	return nil
}

func (opts *CreateOptions) Run(ctx context.Context, args []string) error {
	workdir, err := os.Getwd()
	if err != nil {
		return err
	}

	project, err := compose.NewProjectFromComposeFile(ctx,
		workdir,
		opts.Composefile,
		composespec.WithEnvFiles(opts.EnvFile),
	)
	if err != nil {
		return err
	}

	if err := project.Validate(ctx); err != nil {
		return err
	}

	if err := project.AssignIPs(ctx); err != nil {
		return err
	}

	if opts.RemoveOrphans {
		if err := utils.RemoveOrphans(ctx, project); err != nil {
			return err
		}
	}

	composeController, err := compose.NewComposeProjectV1(ctx)
	if err != nil {
		return err
	}

	embeddedProject, err := composeController.Get(ctx, &composeapi.Compose{
		ObjectMeta: metav1.ObjectMeta{
			Name: project.Name,
		},
	})
	if err != nil {
		return err
	}

	projectMachines := []metav1.ObjectMeta{}
	projectNetworks := []metav1.ObjectMeta{}
	projectVolumes := []metav1.ObjectMeta{}
	if embeddedProject != nil {
		projectMachines = embeddedProject.Status.Machines
		projectNetworks = embeddedProject.Status.Networks
		projectVolumes = embeddedProject.Status.Volumes
	}
	defer func() {
		if _, err := composeController.Update(ctx, &composeapi.Compose{
			ObjectMeta: metav1.ObjectMeta{
				Name: project.Name,
			},
			Spec: composeapi.ComposeSpec{
				Composefile: project.ComposeFiles[0],
				Workdir:     project.WorkingDir,
			},
			Status: composeapi.ComposeStatus{
				Machines: projectMachines,
				Networks: projectNetworks,
				Volumes:  projectVolumes,
			},
		}); err != nil {
			log.G(ctx).WithError(err).Error("failed to update project")
		}
	}()

	networkController, err := mnetwork.NewNetworkV1alpha1ServiceIterator(ctx)
	if err != nil {
		return err
	}

	networks, err := networkController.List(ctx, &networkapi.NetworkList{})
	if err != nil {
		return err
	}

	// We need to first create the networks with a provided subnet
	// and then the ones which we will assign IPs to
	subnetNetworks := []string{}
	emptyNetworks := []string{}
	for name, network := range project.Networks {
		if network.External {
			continue
		}
		if len(network.Ipam.Config) == 0 {
			emptyNetworks = append(emptyNetworks, name)
		} else {
			subnetNetworks = append(subnetNetworks, name)
		}
	}

	orderedNetworks := append(subnetNetworks, emptyNetworks...)

	for _, networkName := range orderedNetworks {
		network := project.Networks[networkName]
		alreadyRunning := false
		for _, n := range networks.Items {
			if n.Name == network.Name {
				alreadyRunning = true
				break
			}
		}
		if alreadyRunning {
			continue
		}

		driver := mnetwork.DefaultStrategyName()
		if network.Driver != "" {
			driver = network.Driver
		}

		subnet := ""
		if len(network.Ipam.Config) > 0 {
			subnet = network.Ipam.Config[0].Subnet
		}
		createOptions := netcreate.CreateOptions{
			Driver:  driver,
			Network: subnet,
		}

		log.G(ctx).Infof("creating network %s...", network.Name)
		if err := createOptions.Run(ctx, []string{network.Name}); err != nil {
			return err
		}

		if network, err := networkController.Get(ctx, &networkapi.Network{
			ObjectMeta: metav1.ObjectMeta{
				Name: network.Name,
			},
		}); err == nil && network.Status.State == networkapi.NetworkStateUp {
			projectNetworks = append(projectNetworks, network.ObjectMeta)
		}

	}

	volumeController, err := mvolume.NewVolumeV1alpha1ServiceIterator(ctx)
	if err != nil {
		return err
	}

	volumes, err := volumeController.List(ctx, &volumeapi.VolumeList{})
	if err != nil {
		return err
	}

	for _, volume := range project.Volumes {
		if volume.External {
			continue
		}
		alreadyExisting := false
		for _, v := range volumes.Items {
			if v.Name == volume.Name {
				alreadyExisting = true
				break
			}
		}
		if alreadyExisting {
			continue
		}

		driver := mvolume.DefaultStrategyName()
		if volume.Driver != "" {
			driver = volume.Driver
		}

		createOptions := volcreate.CreateOptions{
			Driver: driver,
		}

		log.G(ctx).Infof("creating volume %s...", volume.Name)
		if err := createOptions.Run(ctx, []string{volume.Name}); err != nil {
			return err
		}

		volume, err := volumeController.Get(ctx, &volumeapi.Volume{
			ObjectMeta: metav1.ObjectMeta{
				Name: volume.Name,
			},
		})
		if err != nil {
			return err
		}

		if volume != nil {
			projectVolumes = append(projectVolumes, volume.ObjectMeta)
		}

	}

	// Check that none of the services are already running
	machineController, err := mplatform.NewMachineV1alpha1ServiceIterator(ctx)
	if err != nil {
		return err
	}

	machines, err := machineController.List(ctx, &machineapi.MachineList{})
	if err != nil {
		return err
	}

	services, err := project.GetServices(args...)
	if err != nil {
		return err
	}

	orderedServices := project.ServicesOrderedByDependencies(ctx, services, true)
	for _, service := range orderedServices {
		log.G(ctx).Debugf("creating service %s...", service.Name)
		alreadyCreated := false
		for _, machine := range machines.Items {
			if service.ContainerName != machine.Name {
				continue
			}
			if machine.Status.State == machineapi.MachineStateRunning || machine.Status.State == machineapi.MachineStateCreated {
				alreadyCreated = true
				break
			}
			rmOpts := remove.RemoveOptions{
				Platform: machine.Spec.Platform,
			}

			if err := rmOpts.Run(ctx, []string{service.ContainerName}); err != nil {
				return err
			}

			for i, m := range projectMachines {
				if m.Name == machine.Name {
					projectMachines = append(projectMachines[:i], projectMachines[i+1:]...)
					break
				}
			}
			break
		}
		if alreadyCreated {
			continue
		}
		if service.Image == "" {
			if err := buildService(ctx, service); err != nil {
				return err
			}
		} else if err := ensureServiceIsPackaged(ctx, service); err != nil {
			return err
		}

		if err := createService(ctx, project, service); err != nil {
			log.G(ctx).WithError(err).Errorf("failed to create service %s", service.Name)
		}

		if machine, err := machineController.Get(ctx, &machineapi.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: service.ContainerName,
			},
		}); err == nil && machine.Status.State == machineapi.MachineStateCreated {
			projectMachines = append(projectMachines, machine.ObjectMeta)
		} else if err != nil {
			return err
		}
	}

	if _, err := composeController.Update(ctx, &composeapi.Compose{
		ObjectMeta: metav1.ObjectMeta{
			Name: project.Name,
		},
		Spec: composeapi.ComposeSpec{
			Composefile: project.ComposeFiles[0],
			Workdir:     project.WorkingDir,
		},
		Status: composeapi.ComposeStatus{
			Machines: projectMachines,
			Networks: projectNetworks,
			Volumes:  projectVolumes,
		},
	}); err != nil {
		return err
	}

	return nil
}

func ensureServiceIsPackaged(ctx context.Context, service types.ServiceConfig) error {
	plat, arch, err := utils.PlatArchFromService(service)
	if err != nil {
		return err
	}

	parts := strings.SplitN(service.Image, ":", 2)
	imageName := parts[0]
	imageVersion := "latest"
	if len(parts) == 2 {
		imageVersion = parts[1]
	}

	service.Image = imageName + ":" + imageVersion

	log.G(ctx).Debugf("searching for service %s locally...", service.Name)
	// Check whether the image is already in the local catalog
	packages, err := packmanager.G(ctx).Catalog(ctx,
		packmanager.WithArchitecture(arch),
		packmanager.WithName(imageName),
		packmanager.WithPlatform(plat),
		packmanager.WithTypes(unikraft.ComponentTypeApp),
		packmanager.WithVersion(imageVersion))
	if err != nil {
		return err
	}

	// If we have it locally, we are done
	if len(packages) != 0 {
		log.G(ctx).Debugf("found service %s locally", service.Name)
		return nil
	}

	log.G(ctx).Debugf("searching for service %s remotely...", service.Name)
	// Check whether the image is in the remote catalog
	packages, err = packmanager.G(ctx).Catalog(ctx,
		packmanager.WithArchitecture(arch),
		packmanager.WithName(imageName),
		packmanager.WithPlatform(plat),
		packmanager.WithTypes(unikraft.ComponentTypeApp),
		packmanager.WithRemote(true),
		packmanager.WithVersion(imageVersion))
	if err != nil {
		return err
	}

	// If we have it remotely, we are done
	if len(packages) != 0 {
		log.G(ctx).Infof("found service %s remotely, pulling...", service.Name)
		// We need to pull it locally
		pullOptions := pull.PullOptions{Platform: plat, Architecture: arch}
		return pullOptions.Run(ctx, []string{service.Image})
	}

	// Otherwise, we need to build and package it
	if err := buildService(ctx, service); err != nil {
		return err
	}

	return pkgService(ctx, service)
}

func buildService(ctx context.Context, service types.ServiceConfig) error {
	if service.Build == nil {
		return fmt.Errorf("service %s has no build context", service.Name)
	}

	plat, arch, err := utils.PlatArchFromService(service)
	if err != nil {
		return err
	}

	log.G(ctx).Infof("building service %s...", service.Name)

	buildOptions := build.BuildOptions{Platform: plat, Architecture: arch}

	return buildOptions.Run(ctx, []string{service.Build.Context})
}

func pkgService(ctx context.Context, service types.ServiceConfig) error {
	plat, arch, err := utils.PlatArchFromService(service)
	if err != nil {
		return err
	}

	log.G(ctx).Infof("packaging service %s...", service.Name)

	pkgOptions := pkg.PkgOptions{
		Architecture: arch,
		Name:         service.Image,
		Format:       "oci",
		Platform:     plat,
		Strategy:     packmanager.StrategyOverwrite,
	}

	return pkgOptions.Run(ctx, []string{service.Build.Context})
}

func createService(ctx context.Context, project *compose.Project, service types.ServiceConfig) error {
	// The service should be packaged at this point
	plat, arch, err := utils.PlatArchFromService(service)
	if err != nil {
		return err
	}

	log.G(ctx).Infof("creating service %s...", service.Name)

	networks := []string{}
	if len(service.DNS) > 2 {
		log.G(ctx).Warnf("service %s has more than 2 DNS servers, only the first 2 will be used", service.Name)
	}

	dns0 := ""
	dns1 := ""
	if len(service.DNS) > 0 {
		dns0 = service.DNS[0]
	}
	if len(service.DNS) > 1 {
		dns1 = service.DNS[1]
	}
	for name, network := range service.Networks {
		arg := uknetdev.NetdevIp{
			CIDR:     network.Ipv4Address,
			DNS0:     dns0,
			DNS1:     dns1,
			Hostname: service.Hostname,
			Domain:   service.DomainName,
		}
		networks = append(networks, fmt.Sprintf("%s:%s", project.Networks[name].Name, arg.String()))
	}

	volumes := []string{}
	for _, vol := range service.Volumes {
		if volume, ok := project.Volumes[vol.Source]; ok {
			volumes = append(volumes, fmt.Sprintf("%s:%s", volume.Name, vol.Target))
		} else {
			volumes = append(volumes, fmt.Sprintf("%s:%s", vol.Source, vol.Target))
		}
	}

	environ := []string{}
	for k, v := range service.Environment {
		if v == nil {
			environ = append(environ, k)
			continue
		}

		environ = append(environ, fmt.Sprintf("%s=%s", k, *v))
	}

	ports := []string{}
	for _, port := range service.Ports {
		ports = append(ports, fmt.Sprintf("%s:%s:%d/%s", port.HostIP, port.Published, port.Target, port.Protocol))
	}

	memory := ""
	if service.MemLimit > 0 {
		memory = fmt.Sprintf("%d", service.MemLimit)
	} else if service.MemReservation > 0 {
		memory = fmt.Sprintf("%d", service.MemReservation)
	}

	runOptions := run.RunOptions{
		Architecture: arch,
		Detach:       true,
		Env:          environ,
		Memory:       memory,
		Name:         service.ContainerName,
		Networks:     networks,
		NoStart:      true,
		Platform:     plat,
		Ports:        ports,
		Volumes:      volumes,
	}

	if service.Image != "" {
		return runOptions.Run(ctx, []string{service.Image})
	}

	return runOptions.Run(ctx, []string{service.Build.Context})
}
