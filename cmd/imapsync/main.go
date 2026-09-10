// Command imapsync: a two-way folder synchronization daemon between two IMAP
// (or Maildir / EWS) endpoints.
//
// Subcommands:
//
//	imapsync run -config cfg.yaml              run the daemon
//	imapsync db-add-user  -db x.db -name ... -a ... -b ... [-disabled]
//	imapsync db-add-folder -db x.db -a ... -b ...
//	imapsync db-import-yaml -db x.db -config cfg.yaml
//	imapsync db-import-csv  -db x.db [-users u.csv] [-folders f.csv]
//	imapsync db-list -db x.db
//
// Without a subcommand, run is assumed. "imapsync -h" prints this list; "-h" on
// any subcommand prints that subcommand's flags.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	args := os.Args[1:]

	if len(args) > 0 && isHelpFlag(args[0]) {
		usage(os.Stdout)
		return
	}

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
	case "db-remove-user":
		err = cmdDBRemoveUser(args)
	case "db-remove-folder":
		err = cmdDBRemoveFolder(args)
	case "db-forget-user":
		err = cmdDBForgetUser(args)
	case "db-resume-user":
		err = cmdDBResumeUser(args)
	case "db-history":
		err = cmdDBHistory(args)
	case "db-vacuum":
		err = cmdDBVacuum(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}

	if errors.Is(err, flag.ErrHelp) {
		return // "-h" / "-help" on a subcommand: flag already printed its options
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func isHelpFlag(s string) bool {
	return s == "-h" || s == "-help" || s == "--help" || s == "help"
}

func usage(w io.Writer) {
	fmt.Fprint(w, `imapsync - a two-way folder synchronizer

Usage:
  imapsync run -config cfg.yaml
  imapsync db-add-user     -db x.db -name ivanov -a ivanov@a -b ivanov@b [-disabled]
  imapsync db-add-folder   -db x.db -a Sent -b "Sent Items"
  imapsync db-import-yaml   -db x.db -config cfg.yaml
  imapsync db-import-csv    -db x.db [-users users.csv] [-folders folders.csv]
  imapsync db-list         -db x.db
  imapsync db-history      -db x.db -user ivanov [-limit 20]
  imapsync db-remove-user  -db x.db -name ivanov      (removes the user + its state)
  imapsync db-remove-folder -db x.db -a Sent -b "Sent Items"
  imapsync db-forget-user  -db x.db -name ivanov      (reset cache/status, user stays)
  imapsync db-resume-user  -db x.db -name ivanov      (reset error streak, lift the sync stop)
  imapsync db-vacuum       -db x.db

CSV formats:
  users:   name,user_a,user_b[,enabled]   (enabled: 1/0, true/false; default 1)
  folders: folder_a,folder_b
Lines starting with '#' and the header row (name.../folder_a...) are skipped.
`)
}
