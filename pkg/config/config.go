package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	Id             string `json:"Id,omitempty"`
	SshPort        int    `json:"SshPort"`
	SshIdentity    string `json:"SshPriKey"`
	RepoTag        string `json:"Repository"`
	Created        string `json:"Created,omitempty"`
	DiskSize       string `json:"DiskSize,omitempty"`
	Filesystem string `json:"FilesystemType,omitempty"`
	RootSizeMax    string `json:"RootSizeMax,omitempty"`
	Running        bool   `json:"Running,omitempty"`
}

type CacheConfigManager struct {
	CacheDir   string
	ImageID    string
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
		CacheDir:   cacheDir,
		ImageID:    imageId,
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

// Read reads the cache config file and returns the cache cacheConfig
// If the file does not exist, it returns an empty cacheConfig
func (c *CacheConfigManager) Read() (cacheConfig CacheConfig, err error) {
	if _, err = os.Stat(c.configFile); err != nil {
		return cacheConfig, nil
	}

	fileContent, err := os.ReadFile(c.configFile)
	if err != nil {
		return
	}

	if err = json.Unmarshal(fileContent, &cacheConfig); err != nil {
		return
	}

	return
}
