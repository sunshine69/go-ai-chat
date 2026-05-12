package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	_ "github.com/lib/pq"
	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// =============================================================================
// PostgresManager — holds the active connection, safe for concurrent use
// =============================================================================

type PostgresManager struct {
	mu      sync.RWMutex
	db      *sql.DB
	connStr string
}

func NewPostgresManager() *PostgresManager {
	return &PostgresManager{}
}

// connect opens (or re-opens) a connection to the given DSN.
func (m *PostgresManager) connect(connStr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Close existing connection if present
	if m.db != nil {
		_ = m.db.Close()
		m.db = nil
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("failed to open connection: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return fmt.Errorf("connection ping failed: %w", err)
	}

	m.db = db
	m.connStr = connStr
	return nil
}

// getDB returns the active DB or an error if not connected.
func (m *PostgresManager) getDB() (*sql.DB, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.db == nil {
		return nil, fmt.Errorf("no active postgres connection — call postgres_connect first")
	}
	return m.db, nil
}

// =============================================================================
// Tool handlers
// =============================================================================

// postgres_connect — establish (or switch) connection
func (m *PostgresManager) handleConnect(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	connStr, _ := args["connection_string"].(string)
	if connStr == "" {
		// Build from individual fields if no DSN provided
		host, _ := args["host"].(string)
		port, _ := args["port"].(string)
		user, _ := args["user"].(string)
		password, _ := args["password"].(string)
		dbname, _ := args["dbname"].(string)
		sslmode, _ := args["sslmode"].(string)

		if host == "" {
			host = "localhost"
		}
		if port == "" {
			port = "5432"
		}
		if sslmode == "" {
			sslmode = "disable"
		}
		if dbname == "" {
			return mcp.NewToolResultError("provide either connection_string or at least dbname"), nil
		}

		connStr = fmt.Sprintf(
			"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
			host, port, user, password, dbname, sslmode,
		)
	}

	if err := m.connect(connStr); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Redact password from confirmation message
	safe := redactPassword(connStr)
	return mcp.NewToolResultText(fmt.Sprintf("Connected successfully.\nDSN: %s", safe)), nil
}

// postgres_query — read-only SELECT queries; returns rows as JSON
func (m *PostgresManager) handleQuery(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	db, err := m.getDB()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	args := req.GetArguments()
	query, _ := args["query"].(string)
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	// Soft guard: reject obvious mutations
	trimmed := strings.TrimSpace(strings.ToUpper(query))
	for _, kw := range []string{"INSERT", "UPDATE", "DELETE", "DROP", "CREATE", "ALTER", "TRUNCATE"} {
		if strings.HasPrefix(trimmed, kw) {
			return mcp.NewToolResultError(
				fmt.Sprintf("postgres_query is read-only; use postgres_execute for %s statements", kw),
			), nil
		}
	}

	rows, err := db.Query(query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query error: %v", err)), nil
	}
	defer rows.Close()

	result, err := rowsToJSON(rows)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(result), nil
}

// postgres_execute — mutating statements: INSERT / UPDATE / DELETE / DDL
func (m *PostgresManager) handleExecute(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	db, err := m.getDB()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	args := req.GetArguments()
	statement, _ := args["statement"].(string)
	if statement == "" {
		return mcp.NewToolResultError("statement is required"), nil
	}

	confirmed, _ := args["confirmed"].(bool)
	if !confirmed {
		return mcp.NewToolResultText(
			fmt.Sprintf("Confirmation required.\nAbout to execute:\n\n%s\n\nCall again with confirmed=true to proceed.", statement),
		), nil
	}

	res, err := db.Exec(statement)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("execute error: %v", err)), nil
	}

	rowsAffected, _ := res.RowsAffected()
	return mcp.NewToolResultText(fmt.Sprintf("OK — %d row(s) affected.", rowsAffected)), nil
}

// postgres_list_tables — lists all user tables in the connected database
func (m *PostgresManager) handleListTables(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	db, err := m.getDB()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	const q = `
		SELECT table_schema, table_name, table_type
		FROM information_schema.tables
		WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
		ORDER BY table_schema, table_name`

	rows, err := db.Query(q)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer rows.Close()

	result, err := rowsToJSON(rows)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(result), nil
}

// postgres_describe_table — columns, types, nullability, defaults, constraints
func (m *PostgresManager) handleDescribeTable(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	db, err := m.getDB()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	args := req.GetArguments()
	table, _ := args["table"].(string)
	if table == "" {
		return mcp.NewToolResultError("table is required"), nil
	}

	// Support optional schema prefix (schema.table)
	schema := "public"
	if parts := strings.SplitN(table, ".", 2); len(parts) == 2 {
		schema, table = parts[0], parts[1]
	}

	const q = `
		SELECT
			c.column_name,
			c.data_type,
			c.character_maximum_length,
			c.is_nullable,
			c.column_default,
			COALESCE(
				(SELECT string_agg(tc.constraint_type, ', ')
				 FROM information_schema.table_constraints tc
				 JOIN information_schema.key_column_usage kcu
				   ON tc.constraint_name = kcu.constraint_name
				  AND tc.table_schema    = kcu.table_schema
				 WHERE kcu.table_schema  = c.table_schema
				   AND kcu.table_name   = c.table_name
				   AND kcu.column_name  = c.column_name
				), ''
			) AS constraints
		FROM information_schema.columns c
		WHERE c.table_schema = $1
		  AND c.table_name   = $2
		ORDER BY c.ordinal_position`

	rows, err := db.Query(q, schema, table)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer rows.Close()

	result, err := rowsToJSON(rows)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(result), nil
}

// =============================================================================
// Registration
// =============================================================================

func registerPostgresTools(s *server.MCPServer, pg *PostgresManager) {
	// postgres_connect
	s.AddTool(mcp.NewTool("postgres_connect",
		mcp.WithDescription("Connect to a PostgreSQL database. Supply either a full connection_string (DSN) or individual host/port/user/password/dbname fields. Subsequent postgres_* calls use this connection."),
		mcp.WithString("connection_string",
			mcp.Description(`Full postgres DSN, e.g. "postgres://user:pass@host:5432/dbname?sslmode=disable". Takes precedence over individual fields.`),
		),
		mcp.WithString("host", mcp.Description("Hostname or IP (default: localhost)")),
		mcp.WithString("port", mcp.Description("Port (default: 5432)")),
		mcp.WithString("user", mcp.Description("Database user")),
		mcp.WithString("password", mcp.Description("Database password")),
		mcp.WithString("dbname", mcp.Description("Database name")),
		mcp.WithString("sslmode",
			mcp.Description("SSL mode (default: disable)"),
			mcp.Enum("disable", "require", "verify-ca", "verify-full"),
		),
	), pg.handleConnect)

	// postgres_query
	s.AddTool(mcp.NewTool("postgres_query",
		mcp.WithDescription("Run a read-only SQL query (SELECT, EXPLAIN, SHOW, …) against the connected PostgreSQL database. Returns rows as a JSON array."),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("The SQL query to execute. Must be read-only; use postgres_execute for mutations."),
		),
	), pg.handleQuery)

	// postgres_execute
	s.AddTool(mcp.NewTool("postgres_execute",
		mcp.WithDescription("Execute a mutating SQL statement (INSERT, UPDATE, DELETE, CREATE, DROP, ALTER, TRUNCATE, …) against the connected PostgreSQL database. Requires confirmed=true to actually run."),
		mcp.WithString("statement",
			mcp.Required(),
			mcp.Description("The SQL statement to execute."),
		),
		mcp.WithBoolean("confirmed",
			mcp.DefaultBool(false),
			mcp.Description("Must be explicitly set to true to execute. When false, returns a preview and does nothing."),
		),
	), pg.handleExecute)

	// postgres_list_tables
	s.AddTool(mcp.NewTool("postgres_list_tables",
		mcp.WithDescription("List all tables (and views) in the connected PostgreSQL database, grouped by schema."),
	), pg.handleListTables)

	// postgres_describe_table
	s.AddTool(mcp.NewTool("postgres_describe_table",
		mcp.WithDescription(`Describe the columns, types, nullability, defaults and constraints of a table. Use "schema.table" notation to target a non-public schema.`),
		mcp.WithString("table",
			mcp.Required(),
			mcp.Description(`Table name, optionally schema-qualified (e.g. "users" or "billing.invoices").`),
		),
	), pg.handleDescribeTable)
}

// =============================================================================
// Helpers
// =============================================================================

// rowsToJSON converts sql.Rows into a pretty-printed JSON array of objects.
func rowsToJSON(rows *sql.Rows) (string, error) {
	cols, err := rows.Columns()
	if err != nil {
		return "", fmt.Errorf("could not get columns: %w", err)
	}

	var results []map[string]any

	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", fmt.Errorf("scan error: %w", err)
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			// Convert []byte → string for readability
			if b, ok := vals[i].([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = vals[i]
			}
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("rows error: %w", err)
	}

	out, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return "", fmt.Errorf("JSON marshal error: %w", err)
	}
	return fmt.Sprintf("%d row(s)\n%s", len(results), string(out)), nil
}

// redactPassword masks the password in a DSN for safe logging/display.
func redactPassword(dsn string) string {
	// URL form: postgres://user:PASS@host/db
	if idx := strings.Index(dsn, "://"); idx != -1 {
		rest := dsn[idx+3:]
		if at := strings.LastIndex(rest, "@"); at != -1 {
			userInfo := rest[:at]
			if colon := strings.LastIndex(userInfo, ":"); colon != -1 {
				return dsn[:idx+3] + userInfo[:colon] + ":***@" + rest[at+1:]
			}
		}
		return dsn
	}
	// Key-value form: host=... password=SECRET ...
	parts := strings.Fields(dsn)
	for i, p := range parts {
		if strings.HasPrefix(strings.ToLower(p), "password=") {
			parts[i] = "password=***"
		}
	}
	return strings.Join(parts, " ")
}
