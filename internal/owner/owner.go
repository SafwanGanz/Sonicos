package owner

import (
	"strings"
	"sync"

	"github.com/SafwanGanz/Sonicos/internal/aimode"
)

var (
	mu   sync.RWMutex
	list []string
)

func Set(jids []string) {
	mu.Lock()
	defer mu.Unlock()
	list = append([]string(nil), jids...)
}

func List() []string {
	mu.RLock()
	defer mu.RUnlock()
	return append([]string(nil), list...)
}

func IsOwner(sender string) bool {
	mu.RLock()
	owners := list
	mu.RUnlock()
	return aimode.IsOwner(sender, owners)
}

func Configured() bool {
	return len(List()) > 0
}

func Normalize(raw string) string {
	return aimode.NormalizeJID(raw)
}

func ParseList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	return aimode.ParseOwnerList(raw)
}