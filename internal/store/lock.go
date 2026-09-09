package store

import (
	"fmt"
	"os"
	"syscall"
)

// OpenExclusive открывает БД и берёт эксклюзивную advisory-блокировку на
// отдельном lock-файле (<path>.lock). Нужна демону: два процесса на одну БД
// побьют таблицы состояния и статусов. Блокировка снимается в Close.
func OpenExclusive(path string) (*Store, error) {
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("создание lock-файла %s.lock: %w", path, err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lf.Close()
		return nil, fmt.Errorf("БД %s уже используется другим процессом (не удалось взять %s.lock): %w", path, path, err)
	}

	st, err := Open(path)
	if err != nil {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
		return nil, err
	}
	st.lock = lf
	return st, nil
}
