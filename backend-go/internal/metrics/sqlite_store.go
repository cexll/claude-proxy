package metrics

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore SQLite 持久化存储
type SQLiteStore struct {
	db     *sql.DB
	dbPath string

	// 写入缓冲区
	writeBuffer []PersistentRecord
	bufferMu    sync.Mutex
	// 统计：丢弃记录数（缓冲区满/写入失败回退时）
	droppedRecords int64

	// 配置
	batchSize     int           // 批量写入阈值（记录数）
	flushInterval time.Duration // 定时刷新间隔
	retentionDays int           // 数据保留天数

	// 控制
	stopCh  chan struct{}
	wg      sync.WaitGroup
	closed  bool           // 是否已关闭
	flushWg sync.WaitGroup // 追踪异步 flush goroutine
}

// SQLiteStoreConfig SQLite 存储配置
type SQLiteStoreConfig struct {
	DBPath        string // 数据库文件路径
	RetentionDays int    // 数据保留天数（3-30）
}

// 硬编码的内部配置
const (
	defaultBatchSize     = 100              // 批量写入阈值
	defaultFlushInterval = 30 * time.Second // 定时刷新间隔
	maxBufferMultiplier  = 50               // 写入缓冲区上限倍数（相对 batchSize）
	maxFlushRetries      = 3                // flush 写入失败最大重试次数
)

// NewSQLiteStore 创建 SQLite 存储
func NewSQLiteStore(cfg *SQLiteStoreConfig) (*SQLiteStore, error) {
	if cfg == nil {
		cfg = &SQLiteStoreConfig{
			DBPath:        ".config/metrics.db",
			RetentionDays: 7,
		}
	}

	// 验证保留天数范围
	if cfg.RetentionDays < 3 {
		cfg.RetentionDays = 3
	} else if cfg.RetentionDays > 30 {
		cfg.RetentionDays = 30
	}

	// 确保目录存在
	dir := filepath.Dir(cfg.DBPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建数据库目录失败: %w", err)
	}

	// 打开数据库连接（WAL 模式 + NORMAL 同步）
	// modernc.org/sqlite 使用 _pragma= 语法设置 PRAGMA
	dsn := cfg.DBPath + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 设置连接池参数
	db.SetMaxOpenConns(1) // SQLite 单写入连接
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0) // 不限制连接生命周期

	// 初始化表结构
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化数据库 schema 失败: %w", err)
	}

	store := &SQLiteStore{
		db:            db,
		dbPath:        cfg.DBPath,
		writeBuffer:   make([]PersistentRecord, 0, defaultBatchSize*maxBufferMultiplier),
		batchSize:     defaultBatchSize,
		flushInterval: defaultFlushInterval,
		retentionDays: cfg.RetentionDays,
		stopCh:        make(chan struct{}),
	}

	// 启动后台任务
	store.wg.Add(2)
	go store.flushLoop()
	go store.cleanupLoop()

	log.Printf("[SQLite-Init] 指标存储已初始化: %s (保留 %d 天)", cfg.DBPath, cfg.RetentionDays)
	return store, nil
}

// initSchema 初始化数据库表结构
func initSchema(db *sql.DB) error {
	schema := `
		-- 请求记录表
		CREATE TABLE IF NOT EXISTS request_records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			metrics_key TEXT NOT NULL,
			base_url TEXT NOT NULL,
			key_mask TEXT NOT NULL,
			timestamp INTEGER NOT NULL,
			success INTEGER NOT NULL,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_creation_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			api_type TEXT NOT NULL DEFAULT 'messages'
		);

		-- 索引：按 api_type 和时间查询
		CREATE INDEX IF NOT EXISTS idx_records_api_type_timestamp
			ON request_records(api_type, timestamp);

		-- 索引：按 metrics_key 查询
		CREATE INDEX IF NOT EXISTS idx_records_metrics_key
			ON request_records(metrics_key);

		-- 每日预聚合统计表（用于周/月查询加速）
		CREATE TABLE IF NOT EXISTS daily_stats (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			date TEXT NOT NULL,                    -- YYYY-MM-DD (本地日历日)
			api_type TEXT NOT NULL,                -- messages/responses
			metrics_key TEXT NOT NULL,             -- hash(baseURL + apiKey)
			base_url TEXT NOT NULL,
			key_mask TEXT NOT NULL,
			total_requests INTEGER DEFAULT 0,
			success_count INTEGER DEFAULT 0,
			failure_count INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_creation_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			UNIQUE(date, api_type, metrics_key)
		);

		CREATE INDEX IF NOT EXISTS idx_daily_stats_date_api
			ON daily_stats(date, api_type);
	`

	_, err := db.Exec(schema)
	return err
}

// AggregateDailyStats 聚合指定日期（本地日历日）的请求记录到 daily_stats（幂等，可重复执行）
// 注意：仅聚合完整自然日（建议用于 yesterday / 历史日），不要用于正在写入的“今天”。
func (s *SQLiteStore) AggregateDailyStats(day time.Time) error {
	loc := day.Location()
	if loc == nil {
		loc = time.Local
	}

	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1)
	dateStr := start.Format("2006-01-02")

	_, err := s.db.Exec(`
		INSERT INTO daily_stats (
			date, api_type, metrics_key, base_url, key_mask,
			total_requests, success_count, failure_count,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens
		)
		SELECT
			?, api_type, metrics_key, base_url, key_mask,
			COUNT(*) AS total_requests,
			COALESCE(SUM(success), 0) AS success_count,
			COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0) AS failure_count,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
			COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens
		FROM request_records
		WHERE timestamp >= ? AND timestamp < ?
		GROUP BY api_type, metrics_key, base_url, key_mask
		ON CONFLICT(date, api_type, metrics_key) DO UPDATE SET
			base_url = excluded.base_url,
			key_mask = excluded.key_mask,
			total_requests = excluded.total_requests,
			success_count = excluded.success_count,
			failure_count = excluded.failure_count,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			cache_creation_tokens = excluded.cache_creation_tokens,
			cache_read_tokens = excluded.cache_read_tokens
	`, dateStr, start.Unix(), end.Unix())
	if err != nil {
		return fmt.Errorf("聚合 daily_stats 失败 (%s): %w", dateStr, err)
	}
	return nil
}

// AddRecord 添加记录到写入缓冲区（非阻塞）
func (s *SQLiteStore) AddRecord(record PersistentRecord) {
	s.bufferMu.Lock()
	if s.closed {
		s.bufferMu.Unlock()
		return // 已关闭，忽略新记录
	}

	maxBuffer := s.batchSize * maxBufferMultiplier
	if maxBuffer > 0 && len(s.writeBuffer) >= maxBuffer {
		s.droppedRecords++
		s.bufferMu.Unlock()
		return
	}

	s.writeBuffer = append(s.writeBuffer, record)
	shouldFlush := len(s.writeBuffer) >= s.batchSize
	s.bufferMu.Unlock()

	if shouldFlush {
		s.flushWg.Add(1)
		go func() {
			defer s.flushWg.Done()
			s.flush()
		}()
	}
}

// FlushNow 立即将当前缓冲区刷入数据库（同步执行）
// 用于定时聚合/关闭前的最后保障；写入失败会按现有 flush 策略处理并记录日志。
func (s *SQLiteStore) FlushNow() {
	s.flush()
}

// flush 刷新缓冲区到数据库
func (s *SQLiteStore) flush() {
	s.bufferMu.Lock()
	if len(s.writeBuffer) == 0 {
		s.bufferMu.Unlock()
		return
	}

	// 取出缓冲区数据
	records := s.writeBuffer
	s.writeBuffer = make([]PersistentRecord, 0, s.batchSize*maxBufferMultiplier)
	s.bufferMu.Unlock()

	// 批量写入
	if err := s.batchInsertWithRetry(records); err != nil {
		log.Printf("[SQLite-Flush] 警告: 批量写入指标记录失败: %v", err)
		s.requeueOrDropOnFailure(records)
	}
}

func (s *SQLiteStore) batchInsertWithRetry(records []PersistentRecord) error {
	var lastErr error
	backoff := 100 * time.Millisecond
	for attempt := 1; attempt <= maxFlushRetries; attempt++ {
		if err := s.batchInsertRecords(records); err == nil {
			return nil
		} else {
			lastErr = err
		}

		if attempt < maxFlushRetries {
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return lastErr
}

func (s *SQLiteStore) requeueOrDropOnFailure(records []PersistentRecord) {
	s.bufferMu.Lock()
	defer s.bufferMu.Unlock()

	maxBuffer := s.batchSize * maxBufferMultiplier
	if maxBuffer <= 0 {
		s.droppedRecords += int64(len(records))
		return
	}

	// 失败回退时：优先保留 flush 期间新增的记录，再尽量回填本次 flush 的记录（只保留较新的那部分）。
	available := maxBuffer - len(s.writeBuffer)
	if available <= 0 {
		s.droppedRecords += int64(len(records))
		log.Printf("[SQLite-Flush] 警告: 写入缓冲区已满，丢弃 %d 条记录", len(records))
		return
	}

	keep := records
	if len(records) > available {
		// 只保留较新的那部分
		keep = records[len(records)-available:]
		dropped := len(records) - len(keep)
		s.droppedRecords += int64(dropped)
		log.Printf("[SQLite-Flush] 警告: 写入缓冲区容量不足，丢弃 %d 条旧记录", dropped)
	}

	s.writeBuffer = append(keep, s.writeBuffer...)
}

// batchInsertRecords 批量插入记录
func (s *SQLiteStore) batchInsertRecords(records []PersistentRecord) error {
	if len(records) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
		INSERT INTO request_records
		(metrics_key, base_url, key_mask, timestamp, success,
		 input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, api_type)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range records {
		success := 0
		if r.Success {
			success = 1
		}
		_, err := stmt.Exec(
			r.MetricsKey, r.BaseURL, r.KeyMask, r.Timestamp.Unix(), success,
			r.InputTokens, r.OutputTokens, r.CacheCreationTokens, r.CacheReadTokens, r.APIType,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// LoadRecords 加载指定时间范围内的记录
func (s *SQLiteStore) LoadRecords(since time.Time, apiType string) ([]PersistentRecord, error) {
	rows, err := s.db.Query(`
		SELECT metrics_key, base_url, key_mask, timestamp, success,
		       input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens
		FROM request_records
		WHERE timestamp >= ? AND api_type = ?
		ORDER BY timestamp ASC
	`, since.Unix(), apiType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []PersistentRecord
	for rows.Next() {
		var r PersistentRecord
		var ts int64
		var success int

		err := rows.Scan(
			&r.MetricsKey, &r.BaseURL, &r.KeyMask, &ts, &success,
			&r.InputTokens, &r.OutputTokens, &r.CacheCreationTokens, &r.CacheReadTokens,
		)
		if err != nil {
			return nil, err
		}

		r.Timestamp = time.Unix(ts, 0)
		r.Success = success == 1
		r.APIType = apiType
		records = append(records, r)
	}

	return records, rows.Err()
}

// CleanupOldRecords 清理过期数据
func (s *SQLiteStore) CleanupOldRecords(before time.Time) (int64, error) {
	result, err := s.db.Exec(
		"DELETE FROM request_records WHERE timestamp < ?",
		before.Unix(),
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// flushLoop 定时刷新循环
func (s *SQLiteStore) flushLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.flush()
		case <-s.stopCh:
			s.flush() // 关闭前最后一次刷新
			return
		}
	}
}

// cleanupLoop 定期清理循环
func (s *SQLiteStore) cleanupLoop() {
	defer s.wg.Done()

	// 启动时先清理一次
	s.doCleanup()

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.doCleanup()
		case <-s.stopCh:
			return
		}
	}
}

// doCleanup 执行清理
func (s *SQLiteStore) doCleanup() {
	cutoff := time.Now().AddDate(0, 0, -s.retentionDays)
	deleted, err := s.CleanupOldRecords(cutoff)
	if err != nil {
		log.Printf("[SQLite-Cleanup] 警告: 清理过期指标记录失败: %v", err)
	} else if deleted > 0 {
		log.Printf("[SQLite-Cleanup] 已清理 %d 条过期指标记录（超过 %d 天）", deleted, s.retentionDays)
	}
}

// Close 关闭存储
func (s *SQLiteStore) Close() error {
	// 标记为已关闭，阻止新记录
	s.bufferMu.Lock()
	s.closed = true
	s.bufferMu.Unlock()

	// 停止后台循环
	close(s.stopCh)
	s.wg.Wait()

	// 等待所有异步 flush 完成
	s.flushWg.Wait()

	// 关闭前最后尽力刷新（避免 flush 失败导致残留缓冲）
	for i := 0; i < maxFlushRetries; i++ {
		s.bufferMu.Lock()
		empty := len(s.writeBuffer) == 0
		s.bufferMu.Unlock()
		if empty {
			break
		}
		s.flush()
	}

	return s.db.Close()
}

// WriteBufferStats 写入缓冲区统计（用于监控/排查）
type WriteBufferStats struct {
	BufferedRecords    int     `json:"bufferedRecords"`
	MaxBufferRecords   int     `json:"maxBufferRecords"`
	BufferUsage        float64 `json:"bufferUsage"`
	DroppedRecordCount int64   `json:"droppedRecordCount"`
}

func (s *SQLiteStore) GetWriteBufferStats() WriteBufferStats {
	s.bufferMu.Lock()
	defer s.bufferMu.Unlock()

	maxBuffer := s.batchSize * maxBufferMultiplier
	usage := float64(0)
	if maxBuffer > 0 {
		usage = float64(len(s.writeBuffer)) / float64(maxBuffer)
	}

	return WriteBufferStats{
		BufferedRecords:    len(s.writeBuffer),
		MaxBufferRecords:   maxBuffer,
		BufferUsage:        usage,
		DroppedRecordCount: s.droppedRecords,
	}
}

// AggregatedStats 聚合统计（用于 DB 查询返回）
type AggregatedStats struct {
	RequestCount        int64
	SuccessCount        int64
	FailureCount        int64
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
}

func (s *SQLiteStore) QueryRequestRecordTotals(apiType string, start, end time.Time, metricsKeys []string) (AggregatedStats, error) {
	args := []any{apiType, start.Unix(), end.Unix()}

	var b strings.Builder
	b.WriteString(`
		SELECT
			COUNT(*) AS total_requests,
			COALESCE(SUM(success), 0) AS success_count,
			COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0) AS failure_count,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
			COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens
		FROM request_records
		WHERE api_type = ? AND timestamp >= ? AND timestamp < ?
	`)

	if len(metricsKeys) > 0 {
		b.WriteString(" AND metrics_key IN (")
		b.WriteString(strings.TrimRight(strings.Repeat("?,", len(metricsKeys)), ","))
		b.WriteString(")")
		for _, k := range metricsKeys {
			args = append(args, k)
		}
	}

	var out AggregatedStats
	err := s.db.QueryRow(b.String(), args...).Scan(
		&out.RequestCount,
		&out.SuccessCount,
		&out.FailureCount,
		&out.InputTokens,
		&out.OutputTokens,
		&out.CacheCreationTokens,
		&out.CacheReadTokens,
	)
	if err != nil {
		return AggregatedStats{}, err
	}
	return out, nil
}

func (s *SQLiteStore) QueryRequestRecordBucketStats(apiType string, start, end time.Time, interval time.Duration, metricsKeys []string) (map[int64]AggregatedStats, error) {
	intervalSeconds := int64(interval / time.Second)
	if intervalSeconds <= 0 {
		return nil, fmt.Errorf("interval 过小: %s", interval)
	}

	startUnix := start.Unix()
	endUnix := end.Unix()
	args := []any{startUnix, intervalSeconds, apiType, startUnix, endUnix}

	var b strings.Builder
	b.WriteString(`
		SELECT
			CAST((timestamp - ?) / ? AS INTEGER) AS bucket,
			COUNT(*) AS total_requests,
			COALESCE(SUM(success), 0) AS success_count,
			COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0) AS failure_count,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
			COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens
		FROM request_records
		WHERE api_type = ? AND timestamp > ? AND timestamp < ?
	`)

	if len(metricsKeys) > 0 {
		b.WriteString(" AND metrics_key IN (")
		b.WriteString(strings.TrimRight(strings.Repeat("?,", len(metricsKeys)), ","))
		b.WriteString(")")
		for _, k := range metricsKeys {
			args = append(args, k)
		}
	}

	b.WriteString(" GROUP BY bucket ORDER BY bucket ASC")

	rows, err := s.db.Query(b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]AggregatedStats)
	for rows.Next() {
		var bucket int64
		var agg AggregatedStats
		if err := rows.Scan(
			&bucket,
			&agg.RequestCount,
			&agg.SuccessCount,
			&agg.FailureCount,
			&agg.InputTokens,
			&agg.OutputTokens,
			&agg.CacheCreationTokens,
			&agg.CacheReadTokens,
		); err != nil {
			return nil, err
		}
		result[bucket] = agg
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *SQLiteStore) QueryDailyTotals(apiType, startDate, endDate string, metricsKeys []string) (map[string]AggregatedStats, error) {
	args := []any{apiType, startDate, endDate}

	var b strings.Builder
	b.WriteString(`
		SELECT
			date,
			COALESCE(SUM(total_requests), 0) AS total_requests,
			COALESCE(SUM(success_count), 0) AS success_count,
			COALESCE(SUM(failure_count), 0) AS failure_count,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
			COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens
		FROM daily_stats
		WHERE api_type = ? AND date >= ? AND date <= ?
	`)

	if len(metricsKeys) > 0 {
		b.WriteString(" AND metrics_key IN (")
		b.WriteString(strings.TrimRight(strings.Repeat("?,", len(metricsKeys)), ","))
		b.WriteString(")")
		for _, k := range metricsKeys {
			args = append(args, k)
		}
	}

	b.WriteString(" GROUP BY date ORDER BY date ASC")

	rows, err := s.db.Query(b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]AggregatedStats)
	for rows.Next() {
		var dateStr string
		var agg AggregatedStats
		if err := rows.Scan(
			&dateStr,
			&agg.RequestCount,
			&agg.SuccessCount,
			&agg.FailureCount,
			&agg.InputTokens,
			&agg.OutputTokens,
			&agg.CacheCreationTokens,
			&agg.CacheReadTokens,
		); err != nil {
			return nil, err
		}
		result[dateStr] = agg
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// GetRecordCount 获取记录总数（用于调试）
func (s *SQLiteStore) GetRecordCount() (int64, error) {
	var count int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM request_records").Scan(&count)
	return count, err
}
