// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2022, Unikraft GmbH and The KraftKit Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.
package initrd

import (
	"archive/tar"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"kraftkit.sh/cmdfactory"
	"kraftkit.sh/config"
	"kraftkit.sh/cpio"
	"kraftkit.sh/log"

	sfile "github.com/anchore/stereoscope/pkg/file"
	soci "github.com/anchore/stereoscope/pkg/image/oci"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/session/sshforward/sshprovider"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	_ "github.com/moby/buildkit/client/connhelper/dockercontainer"
	_ "github.com/moby/buildkit/client/connhelper/kubepod"
	_ "github.com/moby/buildkit/client/connhelper/nerdctlcontainer"
	_ "github.com/moby/buildkit/client/connhelper/podmancontainer"
	_ "github.com/moby/buildkit/client/connhelper/ssh"
)

var (
	buildArgs    = []string{}
	buildSecrets = []string{}
	buildTarget  string
)

func init() {
	for _, cmd := range []string{
		"kraft build",
		"kraft cloud compose build",
		"kraft cloud compose up",
		"kraft cloud deploy",
		"kraft compose build",
		"kraft compose up",
		"kraft pkg",
	} {
		cmdfactory.RegisterFlag(
			cmd,
			cmdfactory.StringArrayVar(
				&buildArgs,
				"build-arg",
				[]string{},
				"Supply build arguments when building a Dockerfile",
			),
		)
		cmdfactory.RegisterFlag(
			cmd,
			cmdfactory.StringVar(
				&buildTarget,
				"build-target",
				"",
				"Supply multi-stage target when building Dockerfile",
			),
		)
		cmdfactory.RegisterFlag(
			cmd,
			cmdfactory.StringArrayVar(
				&buildSecrets,
				"build-secret",
				[]string{},
				"Supply secrets when building Dockerfile",
			),
		)
	}
}

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
		opts:       InitrdOptions{},
		dockerfile: path,
	}

	for _, opt := range opts {
		if err := opt(&initrd.opts); err != nil {
			return nil, err
		}
	}

	if !filepath.IsAbs(initrd.dockerfile) {
		initrd.dockerfile = filepath.Join(initrd.opts.workdir, initrd.dockerfile)
		if initrd.opts.workdir == "" {
			initrd.opts.workdir = filepath.Dir(initrd.dockerfile)
		}
	} else {
		initrd.opts.workdir = filepath.Dir(initrd.dockerfile)
	}

	fi, err := os.Stat(initrd.dockerfile)
	if err != nil {
		return nil, fmt.Errorf("could not check Dockerfile: %w", err)
	} else if fi.IsDir() {
		return nil, fmt.Errorf("supplied path %s is a directory not a Dockerfile", initrd.dockerfile)
	}

	return &initrd, nil
}

// Build implements Initrd.
func (initrd *dockerfile) Name() string {
	return "Dockerfile"
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
				Image:           "moby/buildkit:v0.18.1",
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

	attrs := map[string]string{
		"filename": filepath.Base(initrd.dockerfile),
	}

	if len(buildTarget) > 0 {
		attrs["target"] = buildTarget
	}

	for _, arg := range buildArgs {
		k, v, ok := strings.Cut(arg, "=")
		if !ok {
			v, ok = os.LookupEnv(k)
			if !ok {
				log.G(ctx).
					WithField("arg", k).
					Warn("could not find build-arg in environment")
				continue
			}
		}

		attrs["build-arg:"+k] = v
	}

	session := []session.Attachable{
		&buildkitAuthProvider{
			config.G[config.KraftKit](ctx).Auth,
		},
	}

	fs := make([]secretsprovider.Source, 0, len(buildSecrets))
	for _, v := range buildSecrets {
		s, err := parseSecret(v)
		if err != nil {
			return "", err
		}
		fs = append(fs, *s)
	}

	secretStore, err := secretsprovider.NewStore(fs)
	if err != nil {
		return "", err
	}

	session = append(session,
		secretsprovider.NewSecretProvider(secretStore),
	)

	sshAgentPath := ""

	// Only a single socket path is supported, prioritize ones targeting kraftkit.
	if p, ok := os.LookupEnv("KRAFTKIT_BUILDKIT_SSH_AGENT"); ok {
		p, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		sshAgentPath = p
	} else if p, ok := os.LookupEnv("SSH_AUTH_SOCK"); ok {
		p, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		sshAgentPath = p
	}
	if len(sshAgentPath) > 0 {
		sshSession, err := sshprovider.NewSSHAgentProvider([]sshprovider.AgentConfig{{
			Paths: []string{sshAgentPath},
		}})
		if err != nil {
			return "", err
		}

		session = append(session,
			sshSession,
		)
	}

	solveOpt := &client.SolveOpt{
		Ref:     identity.NewID(),
		Session: session,
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
		Frontend:      "dockerfile.v0",
		FrontendAttrs: attrs,
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

	// Remove the shell command if it is the first argument
	// TODO(craciunoiuc): Remove this once shell scripts are supported[1]
	// [1]: https://github.com/unikraft/unikraft/pull/1386
	if len(initrd.args) >= 2 && initrd.args[0] == "/bin/sh" && initrd.args[1] == "-c" {
		initrd.args = initrd.args[2:]
	}

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

	type inodeCount struct {
		Count int
		Inode int32
	}
	fileCount := map[string]inodeCount{}

	// Pass once to count links
	for {
		tarHeader, err := tarReader.Next()
		if err == io.EOF {
			break // End of archive
		}
		if err != nil {
			return "", fmt.Errorf("could not read tar header: %w", err)
		}

		if tarHeader.Typeflag == tar.TypeLink {
			if _, ok := fileCount[tarHeader.Linkname]; !ok {
				fileCount[tarHeader.Linkname] = inodeCount{
					Count: 1,
					Inode: rand.Int32(),
				}
			} else {
				fileCount[tarHeader.Linkname] = inodeCount{
					Count: fileCount[tarHeader.Linkname].Count + 1,
					Inode: fileCount[tarHeader.Linkname].Inode,
				}
			}
		} else if tarHeader.Typeflag == tar.TypeReg {
			if _, ok := fileCount[tarHeader.Name]; !ok {
				fileCount[tarHeader.Name] = inodeCount{
					Count: 1,
					Inode: rand.Int32(),
				}
			} else {
				fileCount[tarHeader.Name] = inodeCount{
					Count: fileCount[tarHeader.Name].Count + 1,
					Inode: fileCount[tarHeader.Linkname].Inode,
				}
			}
		}
	}

	_, err = tarArchive.Seek(0, io.SeekStart)
	if err != nil {
		return "", fmt.Errorf("could not seek to start of tarball: %w", err)
	}

	tarReader = tar.NewReader(tarArchive)

	for {
		tarHeader, err := tarReader.Next()
		if err == io.EOF {
			break // End of archive
		}
		if err != nil {
			return "", fmt.Errorf("could not read tar header: %w", err)
		}

		internal := fmt.Sprintf("./%s", filepath.Clean(tarHeader.Name))

		cpioHeader := &cpio.Header{
			Name:    internal,
			Mode:    cpio.FileMode(tarHeader.FileInfo().Mode().Perm()),
			ModTime: tarHeader.FileInfo().ModTime(),
			Size:    tarHeader.FileInfo().Size(),
		}

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
				Trace("symlinking")

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
				Trace("hardlinking")

			cpioHeader.Mode |= cpio.TypeReg
			cpioHeader.Linkname = tarHeader.Linkname
			cpioHeader.Size = 0
			if _, ok := fileCount[tarHeader.Linkname]; ok {
				cpioHeader.Links = fileCount[tarHeader.Linkname].Count
				cpioHeader.Inode = int64(fileCount[tarHeader.Linkname].Inode)
			}
			if err := cpioWriter.WriteHeader(cpioHeader); err != nil {
				return "", fmt.Errorf("could not write CPIO header: %w", err)
			}

		case tar.TypeReg:
			log.G(ctx).
				WithField("src", tarHeader.Name).
				WithField("dst", internal).
				Trace("copying")

			cpioHeader.Mode |= cpio.TypeReg
			cpioHeader.Linkname = tarHeader.Linkname
			cpioHeader.Size = tarHeader.FileInfo().Size()
			if _, ok := fileCount[tarHeader.Name]; ok {
				cpioHeader.Links = fileCount[tarHeader.Name].Count
				cpioHeader.Inode = int64(fileCount[tarHeader.Name].Inode)
			}

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
				Trace("mkdir")

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

type buildkitAuthProvider struct {
	auths map[string]config.AuthConfig
}

func (ap *buildkitAuthProvider) Register(server *grpc.Server) {
	auth.RegisterAuthServer(server, ap)
}

func (ap *buildkitAuthProvider) Credentials(ctx context.Context, req *auth.CredentialsRequest) (*auth.CredentialsResponse, error) {
	res := &auth.CredentialsResponse{}

	if a, ok := ap.auths[req.Host]; ok {
		res.Username = a.User
		res.Secret = a.Token
	}

	return res, nil
}

func (ap *buildkitAuthProvider) FetchToken(ctx context.Context, req *auth.FetchTokenRequest) (*auth.FetchTokenResponse, error) {
	return nil, status.Errorf(codes.Unavailable, "client side tokens disabled")
}

func (ap *buildkitAuthProvider) GetTokenAuthority(ctx context.Context, req *auth.GetTokenAuthorityRequest) (*auth.GetTokenAuthorityResponse, error) {
	return nil, status.Errorf(codes.Unavailable, "client side tokens disabled")
}

func (ap *buildkitAuthProvider) VerifyTokenAuthority(ctx context.Context, req *auth.VerifyTokenAuthorityRequest) (*auth.VerifyTokenAuthorityResponse, error) {
	return nil, status.Errorf(codes.Unavailable, "client side tokens disabled")
}

// parseSecret is derived from [0]
// [0]: https://github.com/moby/buildkit/blob/6737deb443f66e5da79a8ab9a9af36b64b5035cc/cmd/buildctl/build/secret.go#L29-L65
func parseSecret(val string) (*secretsprovider.Source, error) {
	csvReader := csv.NewReader(strings.NewReader(val))
	fields, err := csvReader.Read()
	if err != nil {
		return nil, fmt.Errorf("failed to parse csv secret: %w", err)
	}

	fs := secretsprovider.Source{}

	var typ string
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return nil, fmt.Errorf("invalid field '%s' must be a key=value pair", field)
		}

		key = strings.ToLower(key)
		switch key {
		case "type":
			if value != "file" && value != "env" {
				return nil, fmt.Errorf("unsupported secret type %q", value)
			}
			typ = value
		case "id":
			fs.ID = value
		case "source", "src":
			value, err = filepath.Abs(value)
			if err != nil {
				return nil, fmt.Errorf("secret path '%s' must be absolute: %w", value, err)
			}
			fs.FilePath = value
		case "env":
			fs.Env = value
		default:
			return nil, fmt.Errorf("unexpected key '%s' in '%s'", key, field)
		}
	}

	if typ == "env" && fs.Env == "" {
		fs.Env = fs.FilePath
		fs.FilePath = ""
	}

	return &fs, nil
}
