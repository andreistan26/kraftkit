// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2022, Unikraft GmbH and The KraftKit Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.
package initrd

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sync/errgroup"
	"kraftkit.sh/config"
	"kraftkit.sh/log"

	sfile "github.com/anchore/stereoscope/pkg/file"
	soci "github.com/anchore/stereoscope/pkg/image/oci"
	"github.com/cavaliergopher/cpio"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	_ "github.com/moby/buildkit/client/connhelper/dockercontainer"
	_ "github.com/moby/buildkit/client/connhelper/kubepod"
	_ "github.com/moby/buildkit/client/connhelper/nerdctlcontainer"
	_ "github.com/moby/buildkit/client/connhelper/podmancontainer"
	_ "github.com/moby/buildkit/client/connhelper/ssh"
)

var testcontainersLoggingHook = func(logger testcontainers.Logging) testcontainers.ContainerLifecycleHooks {
	shortContainerID := func(c testcontainers.Container) string {
		return c.GetContainerID()[:12]
	}

	return testcontainers.ContainerLifecycleHooks{
		PreCreates: []testcontainers.ContainerRequestHook{
			func(ctx context.Context, req testcontainers.ContainerRequest) error {
				logger.Printf("creating container for image %s", req.Image)
				return nil
			},
		},
		PostCreates: []testcontainers.ContainerHook{
			func(ctx context.Context, c testcontainers.Container) error {
				logger.Printf("container created: %s", shortContainerID(c))
				return nil
			},
		},
		PreStarts: []testcontainers.ContainerHook{
			func(ctx context.Context, c testcontainers.Container) error {
				logger.Printf("starting container: %s", shortContainerID(c))
				return nil
			},
		},
		PostStarts: []testcontainers.ContainerHook{
			func(ctx context.Context, c testcontainers.Container) error {
				logger.Printf("container started: %s", shortContainerID(c))

				return nil
			},
		},
		PreStops: []testcontainers.ContainerHook{
			func(ctx context.Context, c testcontainers.Container) error {
				logger.Printf("stopping container: %s", shortContainerID(c))
				return nil
			},
		},
		PostStops: []testcontainers.ContainerHook{
			func(ctx context.Context, c testcontainers.Container) error {
				logger.Printf("container stopped: %s", shortContainerID(c))
				return nil
			},
		},
		PreTerminates: []testcontainers.ContainerHook{
			func(ctx context.Context, c testcontainers.Container) error {
				logger.Printf("terminating container: %s", shortContainerID(c))
				return nil
			},
		},
		PostTerminates: []testcontainers.ContainerHook{
			func(ctx context.Context, c testcontainers.Container) error {
				logger.Printf("container terminated: %s", shortContainerID(c))
				return nil
			},
		},
	}
}

type testcontainersPrintf struct {
	ctx context.Context
}

func (t *testcontainersPrintf) Printf(format string, v ...interface{}) {
	if config.G[config.KraftKit](t.ctx).Log.Level == "trace" {
		log.G(t.ctx).Tracef(format, v...)
	}
}

type dockerfile struct {
	opts       InitrdOptions
	args       []string
	dockerfile string
	env        []string
}

func fixedWriteCloser(wc io.WriteCloser) filesync.FileOutputFunc {
	return func(map[string]string) (io.WriteCloser, error) {
		return wc, nil
	}
}

// NewFromDockerfile accepts an input path which represents a Dockerfile that
// can be constructed via buildkit to become a CPIO archive.
func NewFromDockerfile(ctx context.Context, path string, opts ...InitrdOption) (Initrd, error) {
	if !strings.Contains(strings.ToLower(path), "dockerfile") {
		return nil, fmt.Errorf("file is not a Dockerfile")
	}

	initrd := dockerfile{
		opts: InitrdOptions{
			workdir: filepath.Dir(path),
		},
		dockerfile: path,
	}

	for _, opt := range opts {
		if err := opt(&initrd.opts); err != nil {
			return nil, err
		}
	}

	return &initrd, nil
}

// Build implements Initrd.
func (initrd *dockerfile) Build(ctx context.Context) (string, error) {
	if initrd.opts.output == "" {
		fi, err := os.CreateTemp("", "")
		if err != nil {
			return "", err
		}

		initrd.opts.output = fi.Name()
	}

	outputDir, err := os.MkdirTemp("", "")
	if err != nil {
		return "", fmt.Errorf("could not make temporary directory: %w", err)
	}
	defer os.RemoveAll(outputDir)

	tarOutput, err := os.CreateTemp("", "")
	if err != nil {
		return "", fmt.Errorf("could not make temporary file: %w", err)
	}
	defer tarOutput.Close()
	defer os.RemoveAll(tarOutput.Name())

	ociOutput, err := os.CreateTemp("", "")
	if err != nil {
		return "", fmt.Errorf("could not make temporary file: %w", err)
	}
	defer ociOutput.Close()
	defer os.RemoveAll(ociOutput.Name())

	buildkitAddr := config.G[config.KraftKit](ctx).BuildKitHost
	c, _ := client.New(ctx, buildkitAddr)
	buildKitInfo, connerr := c.Info(ctx)
	if connerr != nil {
		log.G(ctx).Info("creating ephemeral buildkit container")

		testcontainers.DefaultLoggingHook = testcontainersLoggingHook
		printf := &testcontainersPrintf{ctx}
		testcontainers.Logger = printf

		// Trap any errors with a helpful message for how to use buildkit
		defer func() {
			if connerr == nil {
				return
			}

			log.G(ctx).Warnf("could not connect to BuildKit client '%s' is BuildKit running?", buildkitAddr)
			log.G(ctx).Warn("")
			log.G(ctx).Warn("By default, KraftKit will look for a native install which")
			log.G(ctx).Warn("is located at /run/buildkit/buildkit.sock.  Alternatively, you")
			log.G(ctx).Warn("can run BuildKit in a container (recommended for macOS users)")
			log.G(ctx).Warn("which you can do by running:")
			log.G(ctx).Warn("")
			log.G(ctx).Warn("  docker run --rm -d --name buildkit --privileged moby/buildkit:latest")
			log.G(ctx).Warn("  export KRAFTKIT_BUILDKIT_HOST=docker-container://buildkit")
			log.G(ctx).Warn("")
			log.G(ctx).Warn("For more usage instructions visit: https://unikraft.org/buildkit")
			log.G(ctx).Warn("")
		}()

		// Port 0 means "give me any free port"
		addr, err := net.ResolveTCPAddr("tcp", ":0")
		if err != nil {
			return "", err
		}
		l, err := net.ListenTCP("tcp", addr)
		if err != nil {
			return "", err
		}

		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()

		buildkitd, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			Started: true,
			Logger:  printf,
			ContainerRequest: testcontainers.ContainerRequest{
				AlwaysPullImage: true,
				Image:           "moby/buildkit:v0.14.1",
				WaitingFor:      wait.ForLog(fmt.Sprintf("running server on [::]:%d", port)),
				Privileged:      true,
				ExposedPorts:    []string{fmt.Sprintf("%d:%d/tcp", port, port)},
				Cmd:             []string{"--addr", fmt.Sprintf("tcp://0.0.0.0:%d", port)},
				Mounts: testcontainers.ContainerMounts{
					{
						Source: testcontainers.GenericVolumeMountSource{
							Name: "kraftkit-buildkit-cache",
						},
						Target: "/var/lib/buildkit",
					},
				},
			},
		})
		if err != nil {
			return "", fmt.Errorf("creating buildkit container: %w", err)
		}

		defer func() {
			if err := buildkitd.Terminate(ctx); err != nil && !strings.Contains(err.Error(), "context cancelled") {
				log.G(ctx).
					WithError(err).
					Debug("terminating buildkit container")
			}
		}()

		buildkitAddr = fmt.Sprintf("tcp://localhost:%d", port)

		c, _ = client.New(ctx, buildkitAddr)
		buildKitInfo, connerr = c.Info(ctx)
		if err != nil {
			return "", fmt.Errorf("connecting to container buildkit client: %w", err)
		}
	}

	log.G(ctx).
		WithField("addr", buildkitAddr).
		WithField("version", buildKitInfo.BuildkitVersion.Version).
		Debug("using buildkit")

	var cacheExports []client.CacheOptionsEntry
	if len(initrd.opts.cacheDir) > 0 {
		cacheExports = []client.CacheOptionsEntry{
			{
				Type: "local",
				Attrs: map[string]string{
					"dest":         initrd.opts.cacheDir,
					"ignore-error": "true",
				},
			},
		}
	}

	solveOpt := &client.SolveOpt{
		Ref: identity.NewID(),
		Exports: []client.ExportEntry{
			{
				Type:   client.ExporterTar,
				Output: fixedWriteCloser(tarOutput),
			},
			{
				Type:   client.ExporterOCI,
				Output: fixedWriteCloser(ociOutput),
			},
		},
		CacheExports: cacheExports,
		LocalDirs: map[string]string{
			"context":    initrd.opts.workdir,
			"dockerfile": initrd.opts.workdir,
		},
		Frontend: "dockerfile.v0",
		FrontendAttrs: map[string]string{
			"filename": filepath.Base(initrd.dockerfile),
		},
	}

	if initrd.opts.arch != "" {
		solveOpt.FrontendAttrs["platform"] = fmt.Sprintf("linux/%s", initrd.opts.arch)
	}

	ch := make(chan *client.SolveStatus)
	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		_, err := c.Solve(ctx, nil, *solveOpt, ch)
		if err != nil {
			return fmt.Errorf("could not solve: %w", err)
		}
		return nil
	})

	eg.Go(func() error {
		d, err := progressui.NewDisplay(log.G(ctx).Writer(), progressui.AutoMode)
		if err != nil {
			return fmt.Errorf("could not create progress display: %w", err)
		}

		_, err = d.UpdateFrom(ctx, ch)
		if err != nil {
			return fmt.Errorf("could not display output progress: %w", err)
		}

		return nil
	})

	if err := eg.Wait(); err != nil {
		return "", fmt.Errorf("could not wait for err group: %w", err)
	}

	// parse the output directory with stereoscope
	tempgen := sfile.NewTempDirGenerator("kraftkit")
	if tempgen == nil {
		return "", fmt.Errorf("could not create temp dir generator")
	}

	provider := soci.NewArchiveProvider(tempgen, ociOutput.Name())
	if provider == nil {
		return "", fmt.Errorf("could not create image provider")
	}

	img, err := provider.Provide(ctx)
	if err != nil {
		return "", fmt.Errorf("could not provide image: %w", err)
	}

	err = img.Read()
	if err != nil {
		return "", fmt.Errorf("could not read image: %w", err)
	}

	initrd.args = append(img.Metadata.Config.Config.Entrypoint,
		img.Metadata.Config.Config.Cmd...,
	)
	initrd.env = img.Metadata.Config.Config.Env

	if err := tempgen.Cleanup(); err != nil {
		return "", fmt.Errorf("could not cleanup temp dir generator: %w", err)
	}

	if err := img.Cleanup(); err != nil {
		return "", fmt.Errorf("could not cleanup image: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(initrd.opts.output), 0o755); err != nil {
		return "", fmt.Errorf("could not create output directory: %w", err)
	}

	cpioFile, err := os.OpenFile(initrd.opts.output, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("could not open initramfs file: %w", err)
	}

	defer cpioFile.Close()

	cpioWriter := cpio.NewWriter(cpioFile)

	defer cpioWriter.Close()

	tarArchive, err := os.Open(tarOutput.Name())
	if err != nil {
		return "", fmt.Errorf("could not open output tarball: %w", err)
	}

	defer tarArchive.Close()

	tarReader := tar.NewReader(tarArchive)

	for {
		tarHeader, err := tarReader.Next()
		if err == io.EOF {
			break // End of archive
		}
		if err != nil {
			return "", fmt.Errorf("could not read tar header: %w", err)
		}

		internal := filepath.Clean(fmt.Sprintf("/%s", tarHeader.Name))

		cpioHeader := &cpio.Header{
			Name:    internal,
			Mode:    cpio.FileMode(tarHeader.FileInfo().Mode().Perm()),
			ModTime: tarHeader.FileInfo().ModTime(),
			Size:    tarHeader.FileInfo().Size(),
		}

		// Populate platform specific information
		populateCPIO(tarHeader.FileInfo(), cpioHeader)

		switch tarHeader.Typeflag {
		case tar.TypeBlock:
			log.G(ctx).
				WithField("file", tarHeader.Name).
				Warn("ignoring block devices")
			continue

		case tar.TypeChar:
			log.G(ctx).
				WithField("file", tarHeader.Name).
				Warn("ignoring char devices")
			continue

		case tar.TypeFifo:
			log.G(ctx).
				WithField("file", tarHeader.Name).
				Warn("ignoring fifo files")
			continue

		case tar.TypeSymlink:
			log.G(ctx).
				WithField("src", tarHeader.Name).
				WithField("link", tarHeader.Linkname).
				Debug("symlinking")

			cpioHeader.Mode |= cpio.TypeSymlink
			cpioHeader.Linkname = tarHeader.Linkname
			cpioHeader.Size = int64(len(tarHeader.Linkname))

			if err := cpioWriter.WriteHeader(cpioHeader); err != nil {
				return "", fmt.Errorf("could not write CPIO header: %w", err)
			}

			if _, err := cpioWriter.Write([]byte(tarHeader.Linkname)); err != nil {
				return "", fmt.Errorf("could not write CPIO data for %s: %w", internal, err)
			}

		case tar.TypeLink:
			log.G(ctx).
				WithField("src", tarHeader.Name).
				WithField("link", tarHeader.Linkname).
				Debug("hardlinking")

			cpioHeader.Mode |= cpio.TypeReg
			cpioHeader.Linkname = tarHeader.Linkname
			cpioHeader.Size = 0
			if err := cpioWriter.WriteHeader(cpioHeader); err != nil {
				return "", fmt.Errorf("could not write CPIO header: %w", err)
			}

		case tar.TypeReg:
			log.G(ctx).
				WithField("src", tarHeader.Name).
				WithField("dst", internal).
				Debug("copying")

			cpioHeader.Mode |= cpio.TypeReg
			cpioHeader.Linkname = tarHeader.Linkname
			cpioHeader.Size = tarHeader.FileInfo().Size()

			if err := cpioWriter.WriteHeader(cpioHeader); err != nil {
				return "", fmt.Errorf("could not write CPIO header: %w", err)
			}

			data, err := io.ReadAll(tarReader)
			if err != nil {
				return "", fmt.Errorf("could not read file: %w", err)
			}

			if _, err := cpioWriter.Write(data); err != nil {
				return "", fmt.Errorf("could not write CPIO data for %s: %w", internal, err)
			}

		case tar.TypeDir:
			log.G(ctx).
				WithField("dst", internal).
				Debug("mkdir")

			cpioHeader.Mode |= cpio.TypeDir

			if err := cpioWriter.WriteHeader(cpioHeader); err != nil {
				return "", fmt.Errorf("could not write CPIO header: %w", err)
			}

		default:
			log.G(ctx).
				WithField("file", tarHeader.Name).
				WithField("type", tarHeader.Typeflag).
				Warn("unsupported file type")
		}
	}

	if initrd.opts.compress {
		if err := compressFiles(initrd.opts.output, cpioWriter, cpioFile); err != nil {
			return "", fmt.Errorf("could not compress files: %w", err)
		}
	}

	return initrd.opts.output, nil
}

// Env implements Initrd.
func (initrd *dockerfile) Env() []string {
	return initrd.env
}

// Args implements Initrd.
func (initrd *dockerfile) Args() []string {
	return initrd.args
}
