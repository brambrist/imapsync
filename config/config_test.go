package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const validCfg = `
server_a:
  host: a.example
  master_user: m
  master_pass: p
server_b:
  host: b.example
  master_user: m
  master_pass: p
folders:
  - a: Sent
    b: Отправленные
users:
  - name: u1
    user_a: u1@a
    user_b: u1@b
  - name: u2
    user_a: u2@a
    user_b: u2@b
workers: 1
sync_interval: 2m
`

func TestLoadValidWithDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, validCfg))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if cfg.ServerA.Port != 993 || cfg.ServerB.Port != 993 {
		t.Errorf("порт по умолчанию не проставлен: %d/%d", cfg.ServerA.Port, cfg.ServerB.Port)
	}
	if cfg.SyncInterval.Std() != 2*time.Minute {
		t.Errorf("sync_interval: got %v", cfg.SyncInterval.Std())
	}
	if cfg.StatsInterval.Std() != time.Minute {
		t.Errorf("stats_interval дефолт: got %v", cfg.StatsInterval.Std())
	}
	if cfg.FetchBatchSize != 200 {
		t.Errorf("fetch_batch_size дефолт: got %d", cfg.FetchBatchSize)
	}
	if cfg.HashHeader != "X-Imapsync-Hash" {
		t.Errorf("hash_header дефолт: got %q", cfg.HashHeader)
	}
}

func TestWorkersMustBeLessThanUsers(t *testing.T) {
	body := validCfg + "" // workers:1, users:2 - ок
	if _, err := Load(writeTemp(t, body)); err != nil {
		t.Fatalf("не ожидали ошибку: %v", err)
	}

	bad := `
server_a: {host: a, master_user: m, master_pass: p}
server_b: {host: b, master_user: m, master_pass: p}
folders: [{a: Sent, b: Sent}]
users:
  - {name: u1, user_a: a, user_b: b}
  - {name: u2, user_a: a, user_b: b}
workers: 2
`
	if _, err := Load(writeTemp(t, bad)); err == nil {
		t.Fatal("ожидали ошибку: workers >= число юзеров")
	}
}

func TestRejectsUnknownField(t *testing.T) {
	bad := validCfg + "\nbogus_field: 1\n"
	if _, err := Load(writeTemp(t, bad)); err == nil {
		t.Fatal("ожидали ошибку на неизвестное поле")
	}
}

func TestRejectsMissingServer(t *testing.T) {
	bad := `
server_b: {host: b, master_user: m, master_pass: p}
folders: [{a: Sent, b: Sent}]
users:
  - {name: u1, user_a: a, user_b: b}
  - {name: u2, user_a: a, user_b: b}
workers: 1
`
	if _, err := Load(writeTemp(t, bad)); err == nil {
		t.Fatal("ожидали ошибку: не задан server_a.host")
	}
}

func TestStateCacheRequiresSQLitePath(t *testing.T) {
	if _, err := Load(writeTemp(t, validCfg+"\nstate_cache: true\n")); err == nil {
		t.Fatal("ожидали ошибку: state_cache без sqlite_path")
	}
	ok := validCfg + "\nstate_cache: true\nsqlite_path: /tmp/x.db\n"
	if _, err := Load(writeTemp(t, ok)); err != nil {
		t.Fatalf("не ожидали ошибку: %v", err)
	}
}

func TestRejectsBadDuration(t *testing.T) {
	bad := validCfg + "\nstats_interval: \"nonsense\"\n"
	if _, err := Load(writeTemp(t, bad)); err == nil {
		t.Fatal("ожидали ошибку разбора длительности")
	}
}
