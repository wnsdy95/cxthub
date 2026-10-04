package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

const NativeWrapperCaptureProtocol = "native-thread-v1"

type nativeWrapperSession struct {
	Version   int    `json:"version"`
	Wrapper   int    `json:"wrapper_pid"`
	SessionID string `json:"session_id"`
}

func nativeWrapperSessionPath(pid int, id string) (string, error) {
	if pid <= 1 || !providerfs.ValidSessionID(id) {
		return "", fmt.Errorf("invalid native wrapper session identity")
	}
	key := domain.HashContent([]byte(strconv.Itoa(pid) + "\x00" + id))
	return filepath.Join(".cxt", "native-wrapper-sessions", strings.TrimPrefix(string(key), "sha256:")+".json"), nil
}

// BindNativeWrapperSession records one owned native thread before activation.
// Native supplies CODEX_THREAD_ID to each tool process after thread creation;
// the app-server's startup environment cannot know that ID beforehand. Both an
// old active thread and its prepared replacement can coexist under one wrapper.
// This local capture binding is not an authentication boundary against the user.
func BindNativeWrapperSession(root string, pid int, id string) (func() error, error) {
	rel, err := nativeWrapperSessionPath(pid, id)
	if err != nil {
		return nil, err
	}
	root = affinityRoot(root)
	raw, err := json.Marshal(nativeWrapperSession{1, pid, id})
	if err != nil {
		return nil, err
	}
	if err = providerfs.WriteRepoFileDurable(root, rel, raw, 0600); err != nil {
		return nil, fmt.Errorf("native wrapper capture binding could not be saved")
	}
	return func() error {
		err := providerfs.RemoveRepoFile(root, rel)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("native wrapper capture binding could not be removed")
		}
		return nil
	}, nil
}

// NativeWrapperSession verifies the provider's exact tool-thread identity.
// The delivery caller must already have verified the live supervisor ancestry.
// Missing/corrupt bindings never fall back to a terminal's older affinity.
func NativeWrapperSession(cwd string, pid int, id string) (string, error) {
	rel, err := nativeWrapperSessionPath(pid, id)
	if err != nil {
		return "", domain.ErrNoActiveSession
	}
	raw, err := providerfs.ReadRepoFile(affinityRoot(cwd), rel)
	var binding nativeWrapperSession
	if err != nil || len(raw) > 1024 || json.Unmarshal(raw, &binding) != nil || binding.Version != 1 || binding.Wrapper != pid || binding.SessionID != id {
		return "", domain.ErrNoActiveSession
	}
	return id, nil
}
