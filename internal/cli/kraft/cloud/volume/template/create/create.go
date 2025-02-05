// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2025, Unikraft GmbH and The KraftKit Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.

package create

import (
	"context"
	"fmt"

	"github.com/MakeNowJust/heredoc"
	"github.com/spf13/cobra"

	kraftcloud "sdk.kraft.cloud"
	kcvolumes "sdk.kraft.cloud/volumes"

	"kraftkit.sh/cmdfactory"
	"kraftkit.sh/config"
	"kraftkit.sh/internal/cli/kraft/cloud/utils"
	"kraftkit.sh/iostreams"
)

type CreateOptions struct {
	Auth   *config.AuthConfig       `noattribute:"true"`
	Client kcvolumes.VolumesService `noattribute:"true"`
	Metro  string                   `noattribute:"true"`
	Token  string                   `noattribute:"true"`
}

// Create a KraftCloud persistent volume.
func Create(ctx context.Context, opts *CreateOptions, args []string) ([]kcvolumes.TemplateCreateResponseItem, error) {
	var err error

	if opts == nil {
		opts = &CreateOptions{}
	}

	if opts.Auth == nil {
		opts.Auth, err = config.GetKraftCloudAuthConfig(ctx, opts.Token)
		if err != nil {
			return nil, fmt.Errorf("could not retrieve credentials: %w", err)
		}
	}

	if opts.Client == nil {
		opts.Client = kraftcloud.NewVolumesClient(
			kraftcloud.WithToken(config.GetKraftCloudTokenAuthConfig(*opts.Auth)),
		)
	}

	createResp, err := opts.Client.WithMetro(opts.Metro).CreateTemplate(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("creating volume: %w", err)
	}
	create, err := createResp.AllOrErr()
	if err != nil {
		return nil, fmt.Errorf("creating volume: %w", err)
	}

	return create, nil
}

func NewCmd() *cobra.Command {
	cmd, err := cmdfactory.New(&CreateOptions{}, cobra.Command{
		Short:   "Create volume template(s)",
		Use:     "create [FLAGS] NAME|UUID[,NAME|UUID...]",
		Args:    cobra.MinimumNArgs(1),
		Aliases: []string{"crt"},
		Long: heredoc.Doc(`
			Create a new persistent volume.
		`),
		Example: heredoc.Doc(`
			# Create a new volume template from "my-volume" volume
			$ kraft cloud volume template create my-volume

			# Create two new volume templates from "my-volume" and "my-other-volume" volumes
			$ kraft cloud volume template create my-volume my-other-volume
		`),
		Annotations: map[string]string{
			cmdfactory.AnnotationHelpGroup: "kraftcloud-volume-template",
		},
	})
	if err != nil {
		panic(err)
	}

	return cmd
}

func (opts *CreateOptions) Pre(cmd *cobra.Command, _ []string) error {
	err := utils.PopulateMetroToken(cmd, &opts.Metro, &opts.Token)
	if err != nil {
		return fmt.Errorf("could not populate metro and token: %w", err)
	}

	return nil
}

func (opts *CreateOptions) Run(ctx context.Context, args []string) error {
	templates, err := Create(ctx, opts, args)
	if err != nil {
		return fmt.Errorf("could not create volume template(s): %w", err)
	}

	for _, template := range templates {
		_, err = fmt.Fprintln(iostreams.G(ctx).Out, template.UUID)
	}

	return err
}
