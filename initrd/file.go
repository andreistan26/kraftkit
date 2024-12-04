// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2022, Unikraft GmbH and The KraftKit Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.
package initrd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

type file struct {
	opts InitrdOptions
	path string
}

// NewFromFile accepts an input file which already represents a CPIO archive and
// is provided as a mechanism for satisfying the Initrd interface.
func NewFromFile(_ context.Context, path string, opts ...InitrdOption) (Initrd, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if stat.IsDir() {
		return nil, fmt.Errorf("path %s is a directory, not a file", path)
	}

	initrd := file{
		opts: InitrdOptions{},
		path: path,
	}

	for _, opt := range opts {
		if err := opt(&initrd.opts); err != nil {
			return nil, err
		}
	}

	absDest, err := filepath.Abs(filepath.Clean(initrd.opts.output))
	if err != nil {
		return nil, fmt.Errorf("getting absolute path of destination: %w", err)
	}

	if absDest == stat.Name() {
		return nil, fmt.Errorf("CPIO archive path is the same as the source path, this is not allowed as it creates corrupted archives")
	}

	return &initrd, nil
}

// Build implements Initrd.
func (initrd *file) Name() string {
	return "file"
}

// Build implements Initrd.
func (initrd *file) Build(_ context.Context) (string, error) {
	return initrd.path, nil
}

// Env implements Initrd.
func (initrd *file) Env() []string {
	return nil
}

// Args implements Initrd.
func (initrd *file) Args() []string {
	return nil
}
