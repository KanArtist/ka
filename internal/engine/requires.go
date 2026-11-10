package engine

import (
	"context"
	"os"
	"path/filepath"

	"github.com/KanArtist/ka/pkg/kaspec"
)

// CodeKaOutdated: a store requires a newer ka (store.yml requires_ka).
const CodeKaOutdated = "ka_outdated"

// requiresKa reads requires_ka from store.yml content ("" when absent).
func requiresKa(data []byte) string {
	sf, _ := kaspec.ParseStoreFile(".ka/store.yml", data)
	if sf == nil {
		return ""
	}
	return sf.RequiresKa
}

func outdated(store, requires string) error {
	if kaspec.KaVersionSatisfies(requires) {
		return nil
	}
	return Errf(CodeKaOutdated, kaspec.UpgradeHint,
		"store %s requires ka %s or newer; this is ka %s, which would misread or damage its data", store, requires, kaspec.KaVersion)
}

// checkVersion refuses a store whose checked-out store.yml requires a newer ka.
func (s *Store) checkVersion() error {
	data, err := os.ReadFile(filepath.Join(s.Root, ".ka", "store.yml"))
	if err != nil {
		return nil // reported elsewhere (invalid_store)
	}
	return outdated(s.Name, requiresKa(data))
}

// checkUpstreamVersion refuses to integrate a remote whose store.yml
// requires a newer ka: the local copy stays as it was, and local commits
// stay unpushed until ka is upgraded.
func (s *Store) checkUpstreamVersion(ctx context.Context, up string) error {
	data, err := s.git.Run(ctx, "show", up+":"+s.repoPath(".ka/store.yml"))
	if err != nil {
		return nil
	}
	return outdated(s.Name, requiresKa([]byte(data+"\n")))
}

// checkVersions refuses a workspace with any store that requires a newer ka.
func checkVersions(stores []*Store) error {
	for _, st := range stores {
		if err := st.checkVersion(); err != nil {
			return err
		}
	}
	return nil
}
