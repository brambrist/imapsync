// Package config отвечает за загрузку, валидацию и заполнение дефолтов
// YAML-конфига синхронизатора.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration - обёртка над time.Duration, умеющая читаться из YAML как строка
// вида "5m", "30s", "10m". Штатный yaml.v3 такой формат не разбирает.
type Duration time.Duration

// UnmarshalYAML разбирает строку через time.ParseDuration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("ожидалась строка длительности (например \"5m\"): %w", err)
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("некорректная длительность %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std возвращает значение как стандартный time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Server описывает один IMAP-сервер и мастер-доступ к нему.
//
// Имперсонация выполняется через SASL PLAIN с authzid: authcid и пароль -
// мастер-учётки, authzid - целевой пользователь (user_a / user_b).
type Server struct {
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	MasterUser string `yaml:"master_user"` // authcid для SASL PLAIN
	MasterPass string `yaml:"master_pass"` // пароль мастер-учётки
}

// FolderPair - явная пара имён папок на сервере A и на сервере B.
// Имена на разных серверах могут отличаться ("Sent" / "Отправленные").
type FolderPair struct {
	A string `yaml:"a"`
	B string `yaml:"b"`
}

// User - один синхронизируемый пользователь: логическое имя для логов и
// адреса (authzid) на каждом из серверов.
type User struct {
	Name  string `yaml:"name"`
	UserA string `yaml:"user_a"`
	UserB string `yaml:"user_b"`
}

// Источники списков юзеров и папок.
const (
	SourceYAML   = "yaml"   // users/folders берутся из этого же YAML
	SourceSQLite = "sqlite" // users/folders берутся из локальной БД sqlite
)

// Config - корневой конфиг.
type Config struct {
	ServerA Server `yaml:"server_a"`
	ServerB Server `yaml:"server_b"`

	// Source - откуда брать списки юзеров и пар папок: "yaml" (по умолчанию)
	// или "sqlite". При "sqlite" секции folders/users в YAML необязательны и
	// игнорируются, а данные читаются из файла SQLitePath.
	Source     string `yaml:"source"`
	SQLitePath string `yaml:"sqlite_path"`

	// Folders/Users заполняются либо из YAML, либо из sqlite (см. Source).
	Folders []FolderPair `yaml:"folders"`
	Users   []User       `yaml:"users"`

	Workers        int      `yaml:"workers"`          // число воркеров, должно быть < числа юзеров
	SyncInterval   Duration `yaml:"sync_interval"`    // пауза между полными циклами
	StatsInterval  Duration `yaml:"stats_interval"`   // периодичность сводной статистики
	PerUserTimeout Duration `yaml:"per_user_timeout"` // таймаут обработки одного юзера
	DialTimeout    Duration `yaml:"dial_timeout"`     // таймаут установки соединения
	FetchBatchSize int      `yaml:"fetch_batch_size"` // размер батча при FETCH
	InsecureTLS    bool     `yaml:"insecure_tls"`     // не проверять сертификат (для тестов)

	// HashHeader - имя кастомного заголовка, куда пишется суррогатный хеш
	// при APPEND, чтобы находить уже скопированные письма на следующих проходах.
	HashHeader string `yaml:"hash_header"`

	// StateCache включает инкрементальную сверку: список UID берётся через
	// UID SEARCH, заголовки фетчатся только для новых писем, разбор кэшируется в
	// sqlite (sqlite_path). Требует заданного sqlite_path.
	StateCache bool `yaml:"state_cache"`
}

// дефолты, применяются к нулевым значениям после парсинга.
const (
	defaultPort           = 993
	defaultWorkers        = 4
	defaultSyncInterval   = Duration(5 * time.Minute)
	defaultStatsInterval  = Duration(1 * time.Minute)
	defaultPerUserTimeout = Duration(10 * time.Minute)
	defaultDialTimeout    = Duration(30 * time.Second)
	defaultFetchBatchSize = 200
	defaultHashHeader     = "X-Imapsync-Hash"
)

// Load читает конфиг из файла, применяет дефолты и валидирует.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("чтение конфига %s: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("разбор конфига %s: %w", path, err)
	}

	cfg.applyDefaults()
	if err := cfg.validateBase(); err != nil {
		return nil, fmt.Errorf("валидация конфига %s: %w", path, err)
	}
	// При source: yaml списки должны быть валидны уже сейчас. При source: sqlite
	// их заполняет и проверяет вызывающий код через ValidateEntities после
	// загрузки из БД.
	if cfg.Source == SourceYAML {
		if err := cfg.ValidateEntities(); err != nil {
			return nil, fmt.Errorf("валидация конфига %s: %w", path, err)
		}
	}
	return &cfg, nil
}

// LoadEntitiesOnly читает YAML и возвращает конфиг с разобранными folders/users
// без валидации базовых полей и режима source. Нужен для команды импорта в
// sqlite, где сам YAML может быть неполным (только списки).
func LoadEntitiesOnly(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("чтение конфига %s: %w", path, err)
	}
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("разбор конфига %s: %w", path, err)
	}
	if len(cfg.Folders) == 0 && len(cfg.Users) == 0 {
		return nil, fmt.Errorf("в %s нет ни folders, ни users для импорта", path)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.ServerA.Port == 0 {
		c.ServerA.Port = defaultPort
	}
	if c.ServerB.Port == 0 {
		c.ServerB.Port = defaultPort
	}
	if c.Workers == 0 {
		c.Workers = defaultWorkers
	}
	if c.SyncInterval == 0 {
		c.SyncInterval = defaultSyncInterval
	}
	if c.StatsInterval == 0 {
		c.StatsInterval = defaultStatsInterval
	}
	if c.PerUserTimeout == 0 {
		c.PerUserTimeout = defaultPerUserTimeout
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = defaultDialTimeout
	}
	if c.FetchBatchSize == 0 {
		c.FetchBatchSize = defaultFetchBatchSize
	}
	if c.HashHeader == "" {
		c.HashHeader = defaultHashHeader
	}
	if c.Source == "" {
		c.Source = SourceYAML
	}
}

// validateBase проверяет всё, что не зависит от источника списков: серверы,
// тайминги, режим источника.
func (c *Config) validateBase() error {
	if err := validateServer("server_a", c.ServerA); err != nil {
		return err
	}
	if err := validateServer("server_b", c.ServerB); err != nil {
		return err
	}

	switch c.Source {
	case SourceYAML:
	case SourceSQLite:
		if strings.TrimSpace(c.SQLitePath) == "" {
			return fmt.Errorf("source: sqlite - требуется sqlite_path")
		}
	default:
		return fmt.Errorf("неизвестный source %q (допустимо: %q, %q)", c.Source, SourceYAML, SourceSQLite)
	}

	if c.StateCache && strings.TrimSpace(c.SQLitePath) == "" {
		return fmt.Errorf("state_cache: true - требуется sqlite_path")
	}

	if c.Workers < 1 {
		return fmt.Errorf("workers должно быть >= 1, задано %d", c.Workers)
	}
	if c.FetchBatchSize < 1 {
		return fmt.Errorf("fetch_batch_size должно быть >= 1, задано %d", c.FetchBatchSize)
	}
	return nil
}

// ValidateEntities проверяет списки папок и юзеров и их согласованность с числом
// воркеров. Вызывается из Load при source: yaml и вручную после загрузки из
// sqlite.
func (c *Config) ValidateEntities() error {
	if len(c.Folders) == 0 {
		return fmt.Errorf("не задано ни одной пары папок (folders)")
	}
	for i, f := range c.Folders {
		if strings.TrimSpace(f.A) == "" || strings.TrimSpace(f.B) == "" {
			return fmt.Errorf("folders[%d]: пустое имя папки (a=%q b=%q)", i, f.A, f.B)
		}
	}

	if len(c.Users) == 0 {
		return fmt.Errorf("не задано ни одного пользователя (users)")
	}
	seen := make(map[string]struct{}, len(c.Users))
	for i, u := range c.Users {
		if strings.TrimSpace(u.Name) == "" {
			return fmt.Errorf("users[%d]: пустое поле name", i)
		}
		if strings.TrimSpace(u.UserA) == "" || strings.TrimSpace(u.UserB) == "" {
			return fmt.Errorf("users[%d] (%s): пустой user_a или user_b", i, u.Name)
		}
		if _, dup := seen[u.Name]; dup {
			return fmt.Errorf("users[%d]: дублирующееся имя %q", i, u.Name)
		}
		seen[u.Name] = struct{}{}
	}

	// Требование из CLAUDE.md: воркеров меньше, чем юзеров.
	if c.Workers >= len(c.Users) && len(c.Users) > 1 {
		return fmt.Errorf("workers (%d) должно быть меньше числа юзеров (%d)", c.Workers, len(c.Users))
	}
	return nil
}

func validateServer(name string, s Server) error {
	if strings.TrimSpace(s.Host) == "" {
		return fmt.Errorf("%s: не задан host", name)
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("%s: некорректный port %d", name, s.Port)
	}
	if strings.TrimSpace(s.MasterUser) == "" {
		return fmt.Errorf("%s: не задан master_user", name)
	}
	if strings.TrimSpace(s.MasterPass) == "" {
		return fmt.Errorf("%s: не задан master_pass", name)
	}
	return nil
}

// Addr возвращает адрес сервера в форме host:port.
func (s Server) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}
