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

// withStore - общий каркас подкоманд db-*: регистрирует флаг -db (и через setup
// любые дополнительные), парсит аргументы, открывает БД и вызывает fn.
func withStore(name string, args []string, setup func(*flag.FlagSet), fn func(*store.Store, *flag.FlagSet) error) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	dbPath := fs.String("db", "", "путь к файлу sqlite")
	if setup != nil {
		setup(fs)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return fmt.Errorf("не задан -db")
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
		name = fs.String("name", "", "логическое имя юзера")
		a = fs.String("a", "", "адрес на сервере A (authzid)")
		b = fs.String("b", "", "адрес на сервере B (authzid)")
		disabled = fs.Bool("disabled", false, "добавить выключенным")
	}, func(st *store.Store, _ *flag.FlagSet) error {
		if err := st.UpsertUser(config.User{Name: *name, UserA: *a, UserB: *b}, !*disabled); err != nil {
			return err
		}
		fmt.Printf("юзер %q сохранён (enabled=%v)\n", *name, !*disabled)
		return nil
	})
}

func cmdDBAddFolder(args []string) error {
	var a, b *string
	return withStore("db-add-folder", args, func(fs *flag.FlagSet) {
		a = fs.String("a", "", "имя папки на сервере A")
		b = fs.String("b", "", "имя папки на сервере B")
	}, func(st *store.Store, _ *flag.FlagSet) error {
		if err := st.UpsertFolderPair(config.FolderPair{A: *a, B: *b}); err != nil {
			return err
		}
		fmt.Printf("пара папок %q/%q сохранена\n", *a, *b)
		return nil
	})
}

func cmdDBImportYAML(args []string) error {
	var cfgPath *string
	return withStore("db-import-yaml", args, func(fs *flag.FlagSet) {
		cfgPath = fs.String("config", "", "путь к YAML-конфигу")
	}, func(st *store.Store, _ *flag.FlagSet) error {
		if *cfgPath == "" {
			return fmt.Errorf("не задан -config")
		}
		// Читаем YAML напрямую, чтобы не спотыкаться о валидацию source: sqlite.
		cfg, err := config.LoadEntitiesOnly(*cfgPath)
		if err != nil {
			return err
		}
		nf, nu, err := st.ImportConfig(cfg)
		if err != nil {
			return err
		}
		fmt.Printf("импортировано из %s: пар папок=%d, юзеров=%d\n", *cfgPath, nf, nu)
		return nil
	})
}

func cmdDBImportCSV(args []string) error {
	var usersCSV, foldersCSV *string
	return withStore("db-import-csv", args, func(fs *flag.FlagSet) {
		usersCSV = fs.String("users", "", "CSV с юзерами: name,user_a,user_b[,enabled]")
		foldersCSV = fs.String("folders", "", "CSV с парами папок: folder_a,folder_b")
	}, func(st *store.Store, _ *flag.FlagSet) error {
		if *usersCSV == "" && *foldersCSV == "" {
			return fmt.Errorf("нужен хотя бы один из -users / -folders")
		}
		if *foldersCSV != "" {
			n, err := importFoldersCSV(st, *foldersCSV)
			if err != nil {
				return err
			}
			fmt.Printf("пар папок импортировано: %d\n", n)
		}
		if *usersCSV != "" {
			n, err := importUsersCSV(st, *usersCSV)
			if err != nil {
				return err
			}
			fmt.Printf("юзеров импортировано: %d\n", n)
		}
		return nil
	})
}

func readCSV(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("открытие %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // допускаем разное число полей (enabled опционален)
	r.TrimLeadingSpace = true

	var rows [][]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("разбор %s: %w", path, err)
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
			return n, fmt.Errorf("%s строка %d: нужно 2 поля (folder_a,folder_b)", path, i+1)
		}
		a, b := strings.TrimSpace(rec[0]), strings.TrimSpace(rec[1])
		if i == 0 && strings.EqualFold(a, "folder_a") {
			continue // заголовок
		}
		if err := st.UpsertFolderPair(config.FolderPair{A: a, B: b}); err != nil {
			return n, fmt.Errorf("%s строка %d: %w", path, i+1, err)
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
			return n, fmt.Errorf("%s строка %d: нужно минимум 3 поля (name,user_a,user_b)", path, i+1)
		}
		name := strings.TrimSpace(rec[0])
		if i == 0 && strings.EqualFold(name, "name") {
			continue // заголовок
		}
		u := config.User{Name: name, UserA: strings.TrimSpace(rec[1]), UserB: strings.TrimSpace(rec[2])}
		enabled := true
		if len(rec) >= 4 {
			enabled = parseBool(rec[3])
		}
		if err := st.UpsertUser(u, enabled); err != nil {
			return n, fmt.Errorf("%s строка %d: %w", path, i+1, err)
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

	fmt.Printf("пары папок (%d):\n", len(folders))
	for _, fp := range folders {
		fmt.Printf("  %q -> %q\n", fp.A, fp.B)
	}
	fmt.Printf("юзеры (%d, включая выключенных):\n", len(users))
	for _, u := range users {
		fmt.Printf("  %s: %s | %s\n", u.Name, u.UserA, u.UserB)
		if s, ok := statuses[u.Name]; ok {
			fmt.Printf("      статус: %s, прогон: %s", s.Status, fmtTime(s.LastRun))
			if !s.LastOK.IsZero() {
				fmt.Printf(", успешно: %s", fmtTime(s.LastOK))
			}
			fmt.Printf("\n      A->B=%d B->A=%d дубли=%d ошибок=%d",
				s.CopiedAToB, s.CopiedBToA, s.SkippedDup, s.Errors)
			if s.LastError != "" {
				fmt.Printf("\n      последняя ошибка: %s", s.LastError)
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
