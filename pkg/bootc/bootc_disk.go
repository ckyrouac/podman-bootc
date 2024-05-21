package bootc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitlab.com/bootc-org/podman-bootc/pkg/cache"
	"gitlab.com/bootc-org/podman-bootc/pkg/config"
	"gitlab.com/bootc-org/podman-bootc/pkg/container"
	"gitlab.com/bootc-org/podman-bootc/pkg/user"

	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/domain/entities/types"
	"github.com/containers/podman/v5/pkg/specgen"
	"github.com/docker/go-units"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// As a baseline heuristic we double the size of
// the input container to support in-place updates.
// This is planned to be more configurable in the
// future.  See also bootc-image-builder
const containerSizeToDiskSizeMultiplier = 2
const diskSizeMinimum = 10 * 1024 * 1024 * 1024 // 10GB
const imageMetaXattr = "user.bootc.meta"

// tempLosetupWrapperContents is a workaround for https://github.com/containers/bootc/pull/487/commits/89d34c7dbcb8a1fa161f812c6ba0a8b49ccbe00f
const tempLosetupWrapperContents = `#!/bin/bash
set -euo pipefail
args=(/usr/sbin/losetup --direct-io=off)
for arg in "$@"; do
	case $arg in
		--direct-io=*) echo "ignoring: $arg" 1>&2;;
		*) args+=("$arg") ;;
	esac
done
exec "${args[@]}"
`

// DiskImageConfig defines configuration for the
type DiskImageConfig struct {
	Filesystem  string
	RootSizeMax string
	DiskSize    string
}

// diskFromContainerMeta is serialized to JSON in a user xattr on a disk image
type diskFromContainerMeta struct {
	// imageDigest is the digested sha256 of the container that was used to build this disk
	ImageDigest string `json:"imageDigest"`
}

type BootcDisk struct {
	ContainerImage          container.ContainerImage
	Cache                   cache.Cache
	User                    user.User
	Ctx                     context.Context
	CreatedAt               time.Time
	file                    *os.File
	bootcInstallContainerId string
	bustCache               bool
	diskImageConfig         DiskImageConfig
	quiet                   bool
	cacheConfig             config.CacheConfig
}

// create singleton for easy cleanup
var (
	instance     *BootcDisk
	instanceOnce sync.Once
)

type BootcDiskParams struct {
	ContainerImage  container.ContainerImage
	Ctx             context.Context
	User            user.User
	Cache           cache.Cache        // the cache directory
	CacheConfig     config.CacheConfig // the cached configuration options
	DiskImageConfig DiskImageConfig    // the disk image options sent by the user
	Quiet           bool
}

// NewBootcDisk creates a new BootcDisk instance
//
// Parameters:
//   - imageNameOrId: the name or id of the container image
//   - ctx: context for the podman machine connection
//   - user: the user who is running the command, determines where the disk image is stored
//   - cache: the cache to use for storing the disk image
//   - bustCache: whether to force a new disk image to be created
func NewBootcDisk(params BootcDiskParams) *BootcDisk {
	instanceOnce.Do(func() {
		instance = &BootcDisk{
			ContainerImage:  params.ContainerImage,
			Ctx:             params.Ctx,
			User:            params.User,
			Cache:           params.Cache,
			diskImageConfig: params.DiskImageConfig,
			quiet:           params.Quiet,
			cacheConfig:     params.CacheConfig,
		}
	})
	return instance
}

// GetSize returns the virtual size of the disk in bytes;
// this may be larger than the actual disk usage
func (p *BootcDisk) GetSize() (int64, error) {
	st, err := os.Stat(p.Cache.GetDiskPath())
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// GetCreatedAt returns the creation time of the disk image
func (p *BootcDisk) GetCreatedAt() time.Time {
	return p.CreatedAt
}

// shouldBustCache compares the cached configuration with the requested configuration
// and updates the bustCache flag if a discrepancy is found
func (p *BootcDisk) shouldBustCache() error {
	p.bustCache = false

	if p.cacheConfig.DiskSize != "" {
		requestedDiskSize, err := p.calculateDiskSize(p.diskImageConfig.DiskSize)
		if err != nil {
			return err
		}

		cachedDiskSize, err := strconv.ParseInt(p.cacheConfig.DiskSize, 10, 64)
		if err != nil {
			return err
		}

		if cachedDiskSize != requestedDiskSize {
			p.bustCache = true
		}
	}

	if p.cacheConfig.Filesystem != p.diskImageConfig.Filesystem {
		p.bustCache = true
	} else if p.cacheConfig.RootSizeMax != p.diskImageConfig.RootSizeMax {
		p.bustCache = true
	}

	return nil
}

func (p *BootcDisk) Install() (err error) {
	p.CreatedAt = time.Now()

	err = p.shouldBustCache()
	if err != nil {
		return
	}

	err = p.getOrInstallImageToDisk()
	if err != nil {
		return
	}

	elapsed := time.Since(p.CreatedAt)
	logrus.Debugf("installImage elapsed: %v", elapsed)

	return
}

func (p *BootcDisk) Cleanup() (err error) {
	force := true
	if p.bootcInstallContainerId != "" {
		_, err := containers.Remove(p.Ctx, p.bootcInstallContainerId, &containers.RemoveOptions{Force: &force})
		if err != nil {
			return fmt.Errorf("failed to remove bootc install container: %w", err)
		}
	}

	return
}

// getOrInstallImageToDisk checks if the disk is present and if not, installs the image to a new disk
func (p *BootcDisk) getOrInstallImageToDisk() error {
	diskPath := p.Cache.GetDiskPath()
	f, err := os.Open(diskPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		logrus.Debugf("No existing disk image found")
		return p.bootcInstallImageToDisk()
	}
	if p.bustCache {
		logrus.Debug("Found existing disk image but cache busting is enabled, removing and recreating")
		err = os.Remove(diskPath)
		if err != nil {
			return err
		}
		return p.bootcInstallImageToDisk()
	}

	logrus.Debug("Found existing disk image, comparing digest")
	defer f.Close()
	buf := make([]byte, 4096)
	len, err := unix.Fgetxattr(int(f.Fd()), imageMetaXattr, buf)
	if err != nil {
		// If there's no xattr, just remove it
		err = os.Remove(diskPath)
		if err != nil {
			return err
		}
		logrus.Debugf("No %s xattr found", imageMetaXattr)
		return p.bootcInstallImageToDisk()
	}
	bufTrimmed := buf[:len]
	var serializedMeta diskFromContainerMeta
	if err := json.Unmarshal(bufTrimmed, &serializedMeta); err != nil {
		logrus.Warnf("failed to parse serialized meta from %s (%v) %v", diskPath, buf, err)
		return p.bootcInstallImageToDisk()
	}

	logrus.Debugf("previous disk digest: %s current digest: %s", serializedMeta.ImageDigest, p.ContainerImage.GetId())
	if serializedMeta.ImageDigest == p.ContainerImage.GetId() {
		return nil
	}

	return p.bootcInstallImageToDisk()
}

func align(size int64, align int64) int64 {
	rem := size % align
	if rem != 0 {
		size += (align - rem)
	}
	return size
}

func (p *BootcDisk) calculateDiskSize(humanSize string) (size int64, err error) {
	size = p.ContainerImage.GetSize() * containerSizeToDiskSizeMultiplier
	if size < diskSizeMinimum {
		size = diskSizeMinimum
	}
	if humanSize != "" {
		diskConfigSize, err := units.FromHumanSize(humanSize)
		if err != nil {
			return -1, err
		}
		if size < diskConfigSize {
			size = diskConfigSize
		}
	}
	// Round up to 4k; loopback wants at least 512b alignment
	size = align(size, 4096)
	return size, nil
}

// bootcInstallImageToDisk creates a disk image from a bootc container
func (p *BootcDisk) bootcInstallImageToDisk() (err error) {
	fmt.Printf("Executing `bootc install to-disk` from container image %s to create disk image\n", p.ContainerImage.GetRepoTag())
	p.file, err = os.CreateTemp(p.Cache.GetDirectory(), "podman-bootc-tempdisk")
	if err != nil {
		return err
	}

	size, err := p.calculateDiskSize(p.diskImageConfig.DiskSize)
	if err != nil {
		return fmt.Errorf("failed to calculate disk size: %w", err)
	}

	humanContainerSize := units.HumanSize(float64(p.ContainerImage.GetSize()))
	humanSize := units.HumanSize(float64(size))
	logrus.Infof("container size: %s, disk size: %s", humanContainerSize, humanSize)

	if err := syscall.Ftruncate(int(p.file.Fd()), size); err != nil {
		return err
	}
	logrus.Debugf("Created %s with size %v", p.file.Name(), size)
	doCleanupDisk := true
	defer func() {
		if doCleanupDisk {
			os.Remove(p.file.Name())
		}
	}()

	err = p.runInstallContainer()
	if err != nil {
		return fmt.Errorf("failed to create disk image: %w", err)
	}
	serializedMeta := diskFromContainerMeta{
		ImageDigest: p.ContainerImage.GetId(),
	}
	buf, err := json.Marshal(serializedMeta)
	if err != nil {
		return err
	}
	if err := unix.Fsetxattr(int(p.file.Fd()), imageMetaXattr, buf, 0); err != nil {
		return fmt.Errorf("failed to set xattr: %w", err)
	}

	diskPath := p.Cache.GetDiskPath()
	if err := os.Rename(p.file.Name(), diskPath); err != nil {
		return fmt.Errorf("failed to rename to %s: %w", diskPath, err)
	}
	doCleanupDisk = false

	return nil
}

// runInstallContainer runs the bootc installer in a container to create a disk image
func (p *BootcDisk) runInstallContainer() (err error) {
	// Create a temporary external shell script with the contents of our losetup wrapper
	losetupTemp, err := os.CreateTemp(p.Cache.GetDirectory(), "losetup-wrapper")
	if err != nil {
		return fmt.Errorf("temp losetup wrapper: %w", err)
	}
	defer os.Remove(losetupTemp.Name())
	if _, err := io.Copy(losetupTemp, strings.NewReader(tempLosetupWrapperContents)); err != nil {
		return fmt.Errorf("temp losetup wrapper copy: %w", err)
	}
	if err := losetupTemp.Chmod(0o755); err != nil {
		return fmt.Errorf("temp losetup wrapper chmod: %w", err)
	}

	createResponse, err := p.createInstallContainer(losetupTemp.Name())
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	p.bootcInstallContainerId = createResponse.ID //save the id for possible cleanup
	logrus.Debugf("Created install container, id=%s", createResponse.ID)

	// run the container to create the disk
	err = containers.Start(p.Ctx, p.bootcInstallContainerId, &containers.StartOptions{})
	if err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}
	logrus.Debugf("Started install container")

	var exitCode int32
	if p.quiet {
		//wait for the container to finish
		logrus.Debugf("Waiting for container completion")
		exitCode, err = containers.Wait(p.Ctx, p.bootcInstallContainerId, nil)
		if err != nil {
			return fmt.Errorf("failed to wait for container: %w", err)
		}
	} else {
		// stream logs to stdout and stderr
		stdOut := make(chan string)
		stdErr := make(chan string)
		logErrors := make(chan error)

		var wg sync.WaitGroup
		go func() {
			follow := true
			defer close(stdOut)
			defer close(stdErr)
			trueV := true
			err = containers.Logs(p.Ctx, p.bootcInstallContainerId, &containers.LogOptions{Follow: &follow, Stdout: &trueV, Stderr: &trueV}, stdOut, stdErr)
			if err != nil {
				logErrors <- err
			}

			close(logErrors)
		}()

		wg.Add(1)
		go func() {
			for str := range stdOut {
				fmt.Print(str)
			}
			wg.Done()
		}()

		wg.Add(1)
		go func() {
			for str := range stdErr {
				fmt.Fprintf(os.Stderr, "%s", str)
			}
			wg.Done()
		}()

		//wait for the container to finish
		logrus.Debugf("Waiting for container completion (streaming output)")
		exitCode, err = containers.Wait(p.Ctx, p.bootcInstallContainerId, nil)
		if err != nil {
			return fmt.Errorf("failed to wait for container: %w", err)
		}

		if err := <-logErrors; err != nil {
			return fmt.Errorf("failed to get logs: %w", err)
		}

		// Ensure the streams are done
		wg.Wait()
	}

	if exitCode != 0 {
		return fmt.Errorf("failed to run bootc install")
	}

	return
}

// createInstallContainer creates a container to run the bootc installer
func (p *BootcDisk) createInstallContainer(tempLosetup string) (createResponse types.ContainerCreateResponse, err error) {
	privileged := true
	autoRemove := true
	labelNested := true

	targetEnv := make(map[string]string)
	if v, ok := os.LookupEnv("BOOTC_INSTALL_LOG"); ok {
		targetEnv["RUST_LOG"] = v
	}

	bootcInstallArgs := []string{
		"bootc", "install", "to-disk", "--via-loopback", "--generic-image",
		"--skip-fetch-check",
	}
	if p.diskImageConfig.Filesystem != "" {
		bootcInstallArgs = append(bootcInstallArgs, "--filesystem", p.diskImageConfig.Filesystem)
	}
	if p.diskImageConfig.RootSizeMax != "" {
		bootcInstallArgs = append(bootcInstallArgs, "--root-size="+p.diskImageConfig.RootSizeMax)
	}
	bootcInstallArgs = append(bootcInstallArgs, "/output/"+filepath.Base(p.file.Name()))

	s := &specgen.SpecGenerator{
		ContainerBasicConfig: specgen.ContainerBasicConfig{
			Command:     bootcInstallArgs,
			PidNS:       specgen.Namespace{NSMode: specgen.Host},
			Remove:      &autoRemove,
			Annotations: map[string]string{"io.podman.annotations.label": "type:unconfined_t"},
			Env:         targetEnv,
		},
		ContainerStorageConfig: specgen.ContainerStorageConfig{
			Image: p.ContainerImage.ImageNameOrId,
			Mounts: []specs.Mount{
				{
					Source:      "/var/lib/containers",
					Destination: "/var/lib/containers",
					Type:        "bind",
				},
				{
					Source:      "/dev",
					Destination: "/dev",
					Type:        "bind",
				},
				{
					Source:      p.Cache.GetDirectory(),
					Destination: "/output",
					Type:        "bind",
				},
				{
					Source: tempLosetup,
					// Note that the default $PATH has /usr/local/sbin first
					Destination: "/usr/local/sbin/losetup",
					Type:        "bind",
					Options:     []string{"ro"},
				},
			},
		},
		ContainerSecurityConfig: specgen.ContainerSecurityConfig{
			Privileged:  &privileged,
			LabelNested: &labelNested,
			SelinuxOpts: []string{"type:unconfined_t"},
		},
		ContainerNetworkConfig: specgen.ContainerNetworkConfig{
			NetNS: specgen.Namespace{
				NSMode: specgen.Bridge,
			},
		},
	}

	createResponse, err = containers.CreateWithSpec(p.Ctx, s, &containers.CreateOptions{})
	if err != nil {
		return createResponse, fmt.Errorf("failed to create container: %w", err)
	}

	return
}
