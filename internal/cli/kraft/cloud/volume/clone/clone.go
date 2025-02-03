// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2023, Unikraft GmbH and The KraftKit Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.

package clone

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

type CloneOptions struct {
	Auth   *config.AuthConfig       `noattribute:"true"`
	Client kcvolumes.VolumesService `noattribute:"true"`
	Metro  string                   `noattribute:"true"`
	Source string                   `local:"true" long:"source" short:"s" usage:"Name or UUID of the source volume or template"`
	Target string                   `local:"true" long:"target" short:"t" usage:"Name or UUID of the target volume"`
	Token  string                   `noattribute:"true"`
}

// Clone a KraftCloud persistent volume.
func Clone(ctx context.Context, opts *CloneOptions) (*kcvolumes.CloneResponseItem, error) {
	var err error

	if opts == nil {
		opts = &CloneOptions{}
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

	cloneResp, err := opts.Client.WithMetro(opts.Metro).Clone(ctx, opts.Source, opts.Target)
	if err != nil {
		return nil, fmt.Errorf("creating volume: %w", err)
	}
	clone, err := cloneResp.FirstOrErr()
	if err != nil {
		return nil, fmt.Errorf("creating volume: %w", err)
	}

	return clone, nil
}

func NewCmd() *cobra.Command {
	cmd, err := cmdfactory.New(&CloneOptions{}, cobra.Command{
		Short:   "Clone a persistent volume or template",
		Use:     "clone [FLAGS]",
		Args:    cobra.NoArgs,
		Aliases: []string{"duplicate", "copy"},
		Long: heredoc.Doc(`
			Create a new persistent volume by cloning an existing
			volume or template.
		`),
		Example: heredoc.Doc(`
			# Create a new persistent volume named "my-volume" by cloning an existing volume "existing-volume"
			$ kraft cloud volume clone --source existing-volume --target my-volume

			# Create a new persistent volume named "my-volume" by cloning an existing template "existing-template"
			$ kraft cloud volume clone -s existing-template -t my-volume
		`),
		Annotations: map[string]string{
			cmdfactory.AnnotationHelpGroup: "kraftcloud-volume",
		},
	})
	if err != nil {
		panic(err)
	}

	return cmd
}

func (opts *CloneOptions) Pre(cmd *cobra.Command, _ []string) error {
	if opts.Source == "" {
		return fmt.Errorf("source volume/template is required")
	}

	err := utils.PopulateMetroToken(cmd, &opts.Metro, &opts.Token)
	if err != nil {
		return fmt.Errorf("could not populate metro and token: %w", err)
	}

	return nil
}

func (opts *CloneOptions) Run(ctx context.Context, _ []string) error {
	volume, err := Clone(ctx, opts)
	if err != nil {
		return fmt.Errorf("could not clone volume: %w", err)
	}

	_, err = fmt.Fprintln(iostreams.G(ctx).Out, volume.UUID)
	return err
}
