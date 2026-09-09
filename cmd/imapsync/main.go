// Команда imapsync: демон двусторонней синхронизации папок между двумя IMAP.
//
// Подкоманды:
//
//	imapsync run -config cfg.yaml              запуск демона
//	imapsync db-add-user  -db x.db -name ... -a ... -b ... [-disabled]
//	imapsync db-add-folder -db x.db -a ... -b ...
//	imapsync db-import-yaml -db x.db -config cfg.yaml
//	imapsync db-import-csv  -db x.db [-users u.csv] [-folders f.csv]
//	imapsync db-list -db x.db
//
// Без подкоманды подразумевается run.
package main

import (
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "db-add-user":
		err = cmdDBAddUser(args)
	case "db-add-folder":
		err = cmdDBAddFolder(args)
	case "db-import-yaml":
		err = cmdDBImportYAML(args)
	case "db-import-csv":
		err = cmdDBImportCSV(args)
	case "db-list":
		err = cmdDBList(args)
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "неизвестная подкоманда %q\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
		os.Exit(1)
	}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func usage() {
	fmt.Fprint(os.Stderr, `imapsync - двусторонний синхронизатор папок IMAP

Использование:
  imapsync run -config cfg.yaml
  imapsync db-add-user   -db x.db -name ivanov -a ivanov@a -b ivanov@b [-disabled]
  imapsync db-add-folder -db x.db -a Sent -b "Отправленные"
  imapsync db-import-yaml -db x.db -config cfg.yaml
  imapsync db-import-csv  -db x.db [-users users.csv] [-folders folders.csv]
  imapsync db-list       -db x.db

CSV-форматы:
  users:   name,user_a,user_b[,enabled]   (enabled: 1/0, true/false; по умолчанию 1)
  folders: folder_a,folder_b
Строки, начинающиеся с '#', и заголовок (name.../folder_a...) пропускаются.
`)
}
