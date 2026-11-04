package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type organization struct {
	Code  string
	Name  string
	Type  string
	Email string
}

func (o organization) planLookupKey(product string) string {
	return sanitizeKey(product) + "_" + sanitizeKey(o.Code)
}

func sanitizeKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

func openEvision() (driver.Conn, string, error) {
	host := os.Getenv("CLICKHOUSE_EVISION_HOST")
	if host == "" {
		return nil, "", fmt.Errorf("CLICKHOUSE_EVISION_HOST is not set")
	}
	port := os.Getenv("CLICKHOUSE_EVISION_PORT")
	if port == "" {
		port = "9000"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, "", fmt.Errorf("CLICKHOUSE_EVISION_PORT %q is not a number", port)
	}
	db := os.Getenv("CLICKHOUSE_EVISION_DATABASE")
	if db == "" {
		db = "evision"
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{host + ":" + port},
		Auth: clickhouse.Auth{
			Database: db,
			Username: os.Getenv("CLICKHOUSE_EVISION_USERNAME"),
			Password: os.Getenv("CLICKHOUSE_EVISION_PASSWORD"),
		},
	})
	if err != nil {
		return nil, "", err
	}
	return conn, db, nil
}

// fetchOrganizations reads the evision organization table. Only code and name
// are required; type and email columns are used when present.
func fetchOrganizations(ctx context.Context, conn driver.Conn, db string) ([]organization, error) {
	cols, err := tableColumns(ctx, conn, db, "organization")
	if err != nil {
		return nil, err
	}
	for _, required := range []string{"code", "name"} {
		if !cols[required] {
			return nil, fmt.Errorf("organization table has no %q column", required)
		}
	}
	selects := []string{"toString(code)", "toString(name)"}
	for _, optional := range []string{"type", "email"} {
		if cols[optional] {
			selects = append(selects, "toString(ifNull("+optional+", ''))")
		} else {
			selects = append(selects, "''")
		}
	}
	where := ""
	if cols["is_active"] {
		where = " WHERE is_active"
	}
	query := "SELECT DISTINCT " + strings.Join(selects, ", ") + " FROM " + db + ".organization" + where + " ORDER BY name"
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query organizations: %w", err)
	}
	defer rows.Close()

	var orgs []organization
	seen := map[string]bool{}
	for rows.Next() {
		var o organization
		if err := rows.Scan(&o.Code, &o.Name, &o.Type, &o.Email); err != nil {
			return nil, err
		}
		o.Code = strings.TrimSpace(o.Code)
		o.Name = strings.Join(strings.Fields(o.Name), " ")
		if o.Code == "" || seen[o.Code] {
			continue
		}
		seen[o.Code] = true
		orgs = append(orgs, o)
	}
	return orgs, rows.Err()
}

func tableColumns(ctx context.Context, conn driver.Conn, db, table string) (map[string]bool, error) {
	rows, err := conn.Query(ctx, "SELECT name FROM system.columns WHERE database = ? AND table = ?", db, table)
	if err != nil {
		return nil, fmt.Errorf("describe %s.%s: %w", db, table, err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("table %s.%s not found", db, table)
	}
	return cols, rows.Err()
}
