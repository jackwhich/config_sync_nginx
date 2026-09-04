package publisher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/infrastructure/fsutil"
)

type owner struct {
	NodeID   string `json:"node_id"`
	Env      string `json:"env"`
	DataDir  string `json:"data_dir"`
	LockFile string `json:"lock_file"`
}

func claim(root *os.Root, name string, want owner) error {
	encoded, e := json.Marshal(want)
	if e != nil {
		return e
	}
	f, e := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e == nil {
		_, e = f.Write(encoded)
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e == nil {
			e = closeErr
		}
		if e == nil {
			e = fsutil.SyncDir(root, ".")
		}
		return e
	}
	if !errors.Is(e, os.ErrExist) {
		return e
	}
	info, e := root.Lstat(name)
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("ownership marker must be a regular file")
	}
	encoded, e = root.ReadFile(name)
	if e != nil {
		return e
	}
	var got owner
	if e = json.Unmarshal(encoded, &got); e != nil {
		return e
	}
	if got != want {
		return fmt.Errorf("ownership mismatch for %s: node_id, env, data_dir and lock_file must remain consistent", name)
	}
	return nil
}
func (r *Runner) claimTarget(t config.Target) error {
	base, e := openTarget(t)
	if e != nil {
		return e
	}
	defer base.Close()
	return claim(base, ".publisher.json", owner{r.cfg.NodeID, r.cfg.Env, r.cfg.DataDir, r.cfg.LockFile})
}
