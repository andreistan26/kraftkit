// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2025, Unikraft GmbH and The KraftKit Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.

package manifest

import "kraftkit.sh/unikraft"

type Manifest struct {
	// Name of the entity which this manifest represents
	Name string `yaml:"name" json:"name"`

	// Type of entity which this manifest represetns
	Type unikraft.ComponentType `yaml:"type" json:"type"`

	// Manifest is used to point to remote manifest, allowing the manifest itself
	// to be retrieved by indirection.  Manifest is XOR with Versions and should
	// be back-propagated.
	Manifest string `yaml:"manifest,omitempty" json:"manifest,omitempty"`

	// Description of what this manifest represents
	Description string `yaml:"description,omitempty" json:"description,omitempty"`

	// Origin represents where (and therefore how) this manifest was populated
	Origin string `yaml:"origin,omitempty" json:"origin,omitempty"`

	// Provider is the string name of the underlying implementation providing the
	// contents of this manifest
	Provider Provider `yaml:"provider,omitempty" json:"provider,omitempty"`

	// Channels provides multiple ways to retrieve versions.  Classically this is
	// a separation between "staging" and "stable"
	Channels []ManifestChannel `yaml:"channels,omitempty" json:"channels,omitempty"`

	// Versions
	Versions []ManifestVersion `yaml:"versions,omitempty" json:"versions,omitempty"`

	// mopts contains additional configuration used within the implementation that
	// are non-exportable attributes and variables.
	mopts *ManifestOptions
}
