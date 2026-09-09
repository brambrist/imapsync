package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"imapsync/config"
	"imapsync/internal/store"
)

// withStore - the common scaffold for db-* subcommands: registers the -db flag
// (and, via setup, any extra ones), parses args, opens the DB and calls fn.
func withStore(name string, args []string, setup func(*flag.FlagSet), fn func(*store.Store, *flag.FlagSet) error) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	dbPath := fs.String("db", "", "path to the sqlite file")
	if setup != nil {
		setup(fs)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return fmt.Errorf("-db is not set")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	return fn(st, fs)
}

func cmdDBAddUser(args []string) error {
	var name, a, b *string
	var disabled *bool
	return withStore("db-add-user", args, func(fs *flag.FlagSet) {
		name = fs.String("name", "", "logical user name")
		a = fs.String("a", "", "address on server A (authzid)")
		b = fs.String("b", "", "address on server B (authzid)")
		disabled = fs.Bool("disabled", false, "add as disabled")
	}, func(st *store.Store, _ *flag.FlagSet) error {
		if err := st.UpsertUser(config.User{Name: *name, UserA: *a, UserB: *b}, !*disabled); err != nil {
			return err
		}
		fmt.Printf("user %q saved (enabled=%v)\n", *name, !*disabled)
		return nil
	})
}

func cmdDBAddFolder(args []string) error {
	var a, b *string
	return withStore("db-add-folder", args, func(fs *flag.FlagSet) {
		a = fs.String("a", "", "folder name on server A")
		b = fs.String("b", "", "folder name on server B")
	}, func(st *store.Store, _ *flag.FlagSet) error {
		if err := st.UpsertFolderPair(config.FolderPair{A: *a, B: *b}); err != nil {
			return err
		}
		fmt.Printf("folder pair %q/%q saved\n", *a, *b)
		return nil
	})
}

func cmdDBImportYAML(args []string) error {
	var cfgPath *string
	return withStore("db-import-yaml", args, func(fs *flag.FlagSet) {
		cfgPath = fs.String("config", "", "path to the YAML config")
	}, func(st *store.Store, _ *flag.FlagSet) error {
		if *cfgPath == "" {
			return fmt.Errorf("-config is not set")
		}
		// Read the YAML directly so the source: sqlite validation does not trip us.
		cfg, err := config.LoadEntitiesOnly(*cfgPath)
		if err != nil {
			return err
		}
		nf, nu, err := st.ImportConfig(cfg)
		if err != nil {
			return err
		}
		fmt.Printf("imported from %s: folder pairs=%d, users=%d\n", *cfgPath, nf, nu)
		return nil
	})
}

func cmdDBImportCSV(args []string) error {
	var usersCSV, foldersCSV *string
	return withStore("db-import-csv", args, func(fs *flag.FlagSet) {
		usersCSV = fs.String("users", "", "users CSV: name,user_a,user_b[,enabled]")
		foldersCSV = fs.String("folders", "", "folder-pairs CSV: folder_a,folder_b")
	}, func(st *store.Store, _ *flag.FlagSet) error {
		if *usersCSV == "" && *foldersCSV == "" {
			return fmt.Errorf("at least one of -users / -folders is required")
		}
		if *foldersCSV != "" {
			n, err := importFoldersCSV(st, *foldersCSV)
			if err != nil {
				return err
			}
			fmt.Printf("folder pairs imported: %d\n", n)
		}
		if *usersCSV != "" {
			n, err := importUsersCSV(st, *usersCSV)
			if err != nil {
				return err
			}
			fmt.Printf("users imported: %d\n", n)
		}
		return nil
	})
}

func readCSV(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // allow a varying field count (enabled is optional)
	r.TrimLeadingSpace = true

	var rows [][]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		if len(rec) == 0 || strings.HasPrefix(strings.TrimSpace(rec[0]), "#") {
			continue
		}
		rows = append(rows, rec)
	}
	return rows, nil
}

func importFoldersCSV(st *store.Store, path string) (int, error) {
	rows, err := readCSV(path)
	if err != nil {
		return 0, err
	}
	n := 0
	for i, rec := range rows {
		if len(rec) < 2 {
			return n, fmt.Errorf("%s line %d: 2 fields required (folder_a,folder_b)", path, i+1)
		}
		a, b := strings.TrimSpace(rec[0]), strings.TrimSpace(rec[1])
		if i == 0 && strings.EqualFold(a, "folder_a") {
			continue // header
		}
		if err := st.UpsertFolderPair(config.FolderPair{A: a, B: b}); err != nil {
			return n, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		n++
	}
	return n, nil
}

func importUsersCSV(st *store.Store, path string) (int, error) {
	rows, err := readCSV(path)
	if err != nil {
		return 0, err
	}
	n := 0
	for i, rec := range rows {
		if len(rec) < 3 {
			return n, fmt.Errorf("%s line %d: at least 3 fields required (name,user_a,user_b)", path, i+1)
		}
		name := strings.TrimSpace(rec[0])
		if i == 0 && strings.EqualFold(name, "name") {
			continue // header
		}
		u := config.User{Name: name, UserA: strings.TrimSpace(rec[1]), UserB: strings.TrimSpace(rec[2])}
		enabled := true
		if len(rec) >= 4 {
			enabled = parseBool(rec[3])
		}
		if err := st.UpsertUser(u, enabled); err != nil {
			return n, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		n++
	}
	return n, nil
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "no", "n", "off", "":
		return false
	default:
		return true
	}
}

func cmdDBRemoveUser(args []string) error {
	var name *string
	return withStore("db-remove-user", args, func(fs *flag.FlagSet) {
		name = fs.String("name", "", "user name")
	}, func(st *store.Store, _ *flag.FlagSet) error {
		if err := st.DeleteUser(*name); err != nil {
			return err
		}
		if err := st.ForgetUser(*name); err != nil {
			return err
		}
		fmt.Printf("user %q removed (along with its cache and history)\n", *name)
		return nil
	})
}

func cmdDBRemoveFolder(args []string) error {
	var a, b *string
	return withStore("db-remove-folder", args, func(fs *flag.FlagSet) {
		a = fs.String("a", "", "folder name on server A")
		b = fs.String("b", "", "folder name on server B")
	}, func(st *store.Store, _ *flag.FlagSet) error {
		if err := st.DeleteFolderPair(config.FolderPair{A: *a, B: *b}); err != nil {
			return err
		}
		fmt.Printf("folder pair %q/%q removed\n", *a, *b)
		return nil
	})
}

func cmdDBResumeUser(args []string) error {
	var name *string
	return withStore("db-resume-user", args, func(fs *flag.FlagSet) {
		name = fs.String("name", "", "user name")
	}, func(st *store.Store, _ *flag.FlagSet) error {
		if err := st.ResumeUser(*name); err != nil {
			return err
		}
		fmt.Printf("error streak for user %q cleared, sync resumes on the next cycle\n", *name)
		return nil
	})
}

func cmdDBForgetUser(args []string) error {
	var name *string
	return withStore("db-forget-user", args, func(fs *flag.FlagSet) {
		name = fs.String("name", "", "user name")
	}, func(st *store.Store, _ *flag.FlagSet) error {
		if err := st.ForgetUser(*name); err != nil {
			return err
		}
		fmt.Printf("state for user %q reset (cache, status, history); the next cycle starts syncing from scratch\n", *name)
		return nil
	})
}

func cmdDBVacuum(args []string) error {
	return withStore("db-vacuum", args, nil, func(st *store.Store, _ *flag.FlagSet) error {
		if err := st.Vacuum(); err != nil {
			return err
		}
		fmt.Println("VACUUM done")
		return nil
	})
}

func cmdDBHistory(args []string) error {
	var name *string
	var limit *int
	return withStore("db-history", args, func(fs *flag.FlagSet) {
		name = fs.String("user", "", "user name")
		limit = fs.Int("limit", 20, "how many recent runs to show")
	}, func(st *store.Store, _ *flag.FlagSet) error {
		if *name == "" {
			return fmt.Errorf("-user is not set")
		}
		runs, err := st.UserRuns(*name, *limit)
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			fmt.Printf("no runs recorded for user %q\n", *name)
			return nil
		}
		fmt.Printf("recent runs for user %q (%d):\n", *name, len(runs))
		for _, r := range runs {
			line := fmt.Sprintf("  %s  %-5s  A->B=%d B->A=%d dups=%d errors=%d",
				fmtTime(r.At), r.Status, r.CopiedAToB, r.CopiedBToA, r.SkippedDup, r.Errors)
			if r.LastError != "" {
				line += "  " + r.LastError
			}
			fmt.Println(line)
		}
		return nil
	})
}

func cmdDBList(args []string) error {
	return withStore("db-list", args, nil, cmdDBListRun)
}

func cmdDBListRun(st *store.Store, _ *flag.FlagSet) error {
	folders, err := st.ListFolderPairs()
	if err != nil {
		return err
	}
	users, err := st.AllUsers()
	if err != nil {
		return err
	}

	statuses, err := st.UserStatuses()
	if err != nil {
		return err
	}

	fmt.Printf("folder pairs (%d):\n", len(folders))
	for _, fp := range folders {
		fmt.Printf("  %q -> %q\n", fp.A, fp.B)
	}
	fmt.Printf("users (%d, including disabled):\n", len(users))
	for _, u := range users {
		fmt.Printf("  %s: %s | %s\n", u.Name, u.UserA, u.UserB)
		if s, ok := statuses[u.Name]; ok {
			fmt.Printf("      status: %s, run: %s", s.Status, fmtTime(s.LastRun))
			if !s.LastOK.IsZero() {
				fmt.Printf(", ok: %s", fmtTime(s.LastOK))
			}
			fmt.Printf("\n      A->B=%d B->A=%d dups=%d errors=%d",
				s.CopiedAToB, s.CopiedBToA, s.SkippedDup, s.Errors)
			if s.FailStreak > 0 {
				fmt.Printf("\n      consecutive errors: %d, streak since %s", s.FailStreak, fmtTime(s.FailSince))
			}
			if s.LastError != "" {
				fmt.Printf("\n      last error: %s", s.LastError)
			}
			fmt.Println()
		}
	}
	return nil
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}
