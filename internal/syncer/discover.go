// Full-mailbox sync (config.Config.SyncAllFolders): discover every folder on
// both sides, pair them up (by SPECIAL-USE attribute first, then by name),
// and create a missing folder on the writable side when direction allows it.
// Not available for ews (it does not implement endpoint.FolderLister).
package syncer

import (
	"fmt"
	"strings"

	"imapsync/config"
	"imapsync/internal/endpoint"
)

// resolveFolders returns the folder pairs to sync for this session: the
// static config list, or - when SyncAllFolders is set - a freshly discovered
// and paired list.
func (s *Syncer) resolveFolders(sess *session) ([]config.FolderPair, error) {
	if !s.cfg.SyncAllFolders {
		return s.cfg.Folders, nil
	}

	listerA, ok := sess.a.(endpoint.FolderLister)
	if !ok {
		return nil, fmt.Errorf("sync_all_folders: %s does not support folder discovery", s.backendA.Addr())
	}
	listerB, ok := sess.b.(endpoint.FolderLister)
	if !ok {
		return nil, fmt.Errorf("sync_all_folders: %s does not support folder discovery", s.backendB.Addr())
	}

	foldersA, err := listerA.ListFolders()
	if err != nil {
		return nil, fmt.Errorf("sync_all_folders, side A: %w", err)
	}
	foldersB, err := listerB.ListFolders()
	if err != nil {
		return nil, fmt.Errorf("sync_all_folders, side B: %w", err)
	}

	return s.pairFolders(sess, foldersA, foldersB), nil
}

// pairFolders filters both folder lists, matches them, and creates a missing
// folder on the writable side (honouring direction and ReadOnly - the same
// rule syncPair itself uses for messages).
func (s *Syncer) pairFolders(sess *session, foldersA, foldersB []endpoint.FolderInfo) []config.FolderPair {
	u := sess.u
	foldersA = filterFolders(foldersA, s.cfg.FolderExclude)
	foldersB = filterFolders(foldersB, s.cfg.FolderExclude)

	pairs, onlyA, onlyB := matchFolders(foldersA, foldersB)

	allowAToB := s.dirAToB && !sess.b.ReadOnly()
	allowBToA := s.dirBToA && !sess.a.ReadOnly()

	if creator, ok := sess.b.(endpoint.FolderCreator); ok && allowAToB {
		for _, fa := range onlyA {
			if err := creator.CreateFolder(fa.Name); err != nil {
				s.logf("user %s, sync_all_folders: creating %q on B failed: %v - skipped", u.Name, fa.Name, err)
				continue
			}
			pairs = append(pairs, config.FolderPair{A: fa.Name, B: fa.Name})
		}
	} else if len(onlyA) > 0 {
		s.logf("user %s, sync_all_folders: %d folder(s) exist only on A, not mirrored to B (direction, read-only, or B cannot create folders)",
			u.Name, len(onlyA))
	}

	if creator, ok := sess.a.(endpoint.FolderCreator); ok && allowBToA {
		for _, fb := range onlyB {
			if err := creator.CreateFolder(fb.Name); err != nil {
				s.logf("user %s, sync_all_folders: creating %q on A failed: %v - skipped", u.Name, fb.Name, err)
				continue
			}
			pairs = append(pairs, config.FolderPair{A: fb.Name, B: fb.Name})
		}
	} else if len(onlyB) > 0 {
		s.logf("user %s, sync_all_folders: %d folder(s) exist only on B, not mirrored to A (direction, read-only, or A cannot create folders)",
			u.Name, len(onlyB))
	}

	return pairs
}

// filterFolders drops non-selectable (structural-only) folders and anything
// matching FolderExclude, by name or SPECIAL-USE token (case-insensitive).
func filterFolders(folders []endpoint.FolderInfo, exclude []string) []endpoint.FolderInfo {
	out := make([]endpoint.FolderInfo, 0, len(folders))
	for _, f := range folders {
		if !f.Selectable || isExcluded(f, exclude) {
			continue
		}
		out = append(out, f)
	}
	return out
}

func isExcluded(f endpoint.FolderInfo, exclude []string) bool {
	for _, ex := range exclude {
		ex = strings.TrimSpace(ex)
		if ex == "" {
			continue
		}
		if strings.EqualFold(ex, f.Name) {
			return true
		}
		if f.SpecialUse != "" && strings.EqualFold(ex, f.SpecialUse) {
			return true
		}
	}
	return false
}

// matchFolders pairs folders from a and b: first by SPECIAL-USE attribute
// (so "Sent" on one side pairs with "Sent Items" on the other), then by
// case-insensitive name. Returns the pairs plus what is left over on each
// side.
func matchFolders(a, b []endpoint.FolderInfo) (pairs []config.FolderPair, onlyA, onlyB []endpoint.FolderInfo) {
	matchedA := make([]bool, len(a))
	usedB := make([]bool, len(b))

	for i, fa := range a {
		if fa.SpecialUse == "" {
			continue
		}
		for j, fb := range b {
			if usedB[j] || fb.SpecialUse == "" || !strings.EqualFold(fa.SpecialUse, fb.SpecialUse) {
				continue
			}
			pairs = append(pairs, config.FolderPair{A: fa.Name, B: fb.Name})
			matchedA[i], usedB[j] = true, true
			break
		}
	}
	for i, fa := range a {
		if matchedA[i] {
			continue
		}
		for j, fb := range b {
			if usedB[j] || !strings.EqualFold(fa.Name, fb.Name) {
				continue
			}
			pairs = append(pairs, config.FolderPair{A: fa.Name, B: fb.Name})
			matchedA[i], usedB[j] = true, true
			break
		}
	}

	for i, fa := range a {
		if !matchedA[i] {
			onlyA = append(onlyA, fa)
		}
	}
	for j, fb := range b {
		if !usedB[j] {
			onlyB = append(onlyB, fb)
		}
	}
	return pairs, onlyA, onlyB
}
