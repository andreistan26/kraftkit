// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2022, Unikraft GmbH and The KraftKit Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.
package oci

import (
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Convert github.com/google/go-containerregistry/pkg/v1.Descriptor to
// github.com/opencontainers/image-spec/specs-go/v1.Descriptor
func FromGoogleV1DescriptorToOCISpec(from ...v1.Descriptor) []ocispec.Descriptor {
	to := make([]ocispec.Descriptor, len(from))

	for i := range from {
		to[i] = ocispec.Descriptor{
			MediaType:   string(from[i].MediaType),
			Digest:      digest.Digest(from[i].Digest.String()),
			Size:        from[i].Size,
			URLs:        from[i].URLs,
			Annotations: from[i].Annotations,
			Data:        from[i].Data,
			Platform:    FromGoogleV1PlatformToOCISpec(from[i].Platform),
		}
	}

	return to
}

// Convert github.com/google/go-containerregistry/pkg/v1.Platform to
// github.com/opencontainers/image-spec/specs-go/v1.Platform
func FromGoogleV1PlatformToOCISpec(from *v1.Platform) *ocispec.Platform {
	if from == nil {
		return nil
	}

	return &ocispec.Platform{
		Architecture: from.Architecture,
		OS:           from.OS,
		OSVersion:    from.OSVersion,
		OSFeatures:   from.OSFeatures,
		Variant:      from.Variant,
	}
}
