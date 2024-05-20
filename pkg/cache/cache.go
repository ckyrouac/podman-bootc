package cache

import (
	"fmt"
	"os"
	"path/filepath"

	"gitlab.com/bootc-org/podman-bootc/pkg/user"
	"gitlab.com/bootc-org/podman-bootc/pkg/utils"
)

// NewCache creates a new cache object
// Parameters:
//     - id: the full or partial image ID. If partial, it will be expanded to the full image ID
//     - user: the user who is running the podman-bootc command
func NewCache(id string, user user.User) (cache Cache, err error) {
	fullImageId, err := utils.FullImageIdFromPartial(id, user)
	if err != nil {
		return Cache{}, err
	}

	return Cache{
		ImageId: fullImageId,
		User:    user,
	}, nil
}

type Cache struct {
	User      user.User
	ImageId   string
	Directory string
	Created	  bool
	lock      CacheLock
}

// Create VM cache dir; one per oci bootc image
func (p *Cache) Create() (err error) {
	p.Directory = filepath.Join(p.User.CacheDir(), p.ImageId)

	if err := os.MkdirAll(p.Directory, os.ModePerm); err != nil {
		return fmt.Errorf("error while making bootc disk directory: %w", err)
	}

	p.Created = true

	return
}

func (p *Cache) GetDirectory() string {
	if !p.Created {
		panic("cache not created")
	}
	return p.Directory
}

func (p *Cache) GetDiskPath() string {
	if !p.Created {
		panic("cache not created")
	}
	return filepath.Join(p.GetDirectory(), "disk.raw")
}

func (p *Cache) Lock(mode AccessMode) error {
	p.lock = NewCacheLock(p.User.RunDir(), p.Directory)
	locked, err := p.lock.TryLock(Exclusive)
	if err != nil {
		return fmt.Errorf("error locking the cache path: %w", err)
	}
	if !locked {
		return fmt.Errorf("unable to lock the cache path")
	}

	return nil
}

func (p *Cache) Unlock() error {
	return p.lock.Unlock()
}

