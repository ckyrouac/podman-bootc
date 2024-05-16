package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/go-units"
)

const (
	ProjectName      = "podman-bootc"
	CacheDir         = ".cache"
	RunPidFile       = "run.pid"
	OciArchiveOutput = "image-archive.tar"
	DiskImage        = "disk.raw"
	CiDataIso        = "cidata.iso"
	SshKeyFile       = "sshkey"
	CfgFile          = "bc.cfg"
	LibvirtUri       = "qemu:///session"
)

type CacheConfig struct {
	Id          string `json:"Id,omitempty"`
	SshPort     int    `json:"SshPort"`
	SshIdentity string `json:"SshPriKey"`
	RepoTag     string `json:"Repository"`
	Created     string `json:"Created,omitempty"`
	DiskSize    string `json:"DiskSize,omitempty"`
	Running     bool   `json:"Running,omitempty"`
}

type CacheConfigManager struct {
	CacheDir string
	ImageID  string
	configFile string
}

func NewCacheConfigManager(cacheDir string, imageId string) (*CacheConfigManager, error) {
	caches, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("error reading cache directory: %w", err)
	}

	fullImageId := ""
	for _, cache := range caches {
		if cache.IsDir() && strings.Index(cache.Name(), imageId) == 0 {
			fullImageId = cache.Name()
		}
	}

	if fullImageId == "" {
		return nil, fmt.Errorf("cache directory for image %s not found", imageId)
	}

	return &CacheConfigManager{
		CacheDir: cacheDir,
		ImageID:  imageId,
		configFile: filepath.Join(cacheDir, fullImageId, CfgFile),
	}, nil
}

func (c *CacheConfigManager) Write(cacheConfig CacheConfig) error {
	configJson, err := json.Marshal(cacheConfig)
	if err != nil {
		return fmt.Errorf("marshal config data: %w", err)
	}
	err = os.WriteFile(c.configFile, configJson, 0660)
	if err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	return nil
}

func (c *CacheConfigManager) Read() (cacheConfig CacheConfig, err error) {
	fileContent, err := os.ReadFile(c.configFile)
	if err != nil {
		return
	}

	if err = json.Unmarshal(fileContent, &cacheConfig); err != nil {
		return
	}

	//format the config values for display
	createdTime, err := time.Parse(time.RFC3339, cacheConfig.Created)
	if err != nil {
		return cacheConfig, fmt.Errorf("error parsing created time: %w", err)
	}
	cacheConfig.Created = units.HumanDuration(time.Since(createdTime)) + " ago"

	diskSizeFloat, err := strconv.ParseFloat(cacheConfig.DiskSize, 64)
	if err != nil {
		return cacheConfig, fmt.Errorf("error parsing disk size: %w", err)
	}
	cacheConfig.DiskSize = units.HumanSizeWithPrecision(diskSizeFloat, 3)

	return
}

