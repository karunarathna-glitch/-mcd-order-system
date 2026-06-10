package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB

const (
	StatusReceived  = "オーダ受信済み"
	StatusCooked    = "調理済み"
	StatusDelivered = "受け渡し済み"
)

// OrderItem はデータベースのレコードマッピング用
type OrderItem struct {
	ID          int64     `json:"id"`
	OrderNo     string    `json:"orderNo"`
	TerminalNo  string    `json:"terminalNo"`
	OrderStatus string    `json:"orderStatus"`
	ItemNo      int       `json:"itemNo"`
	MenuName    string    `json:"menuName"`
	UnitPrice   int       `json:"unitPrice"`
	Quantity    int       `json:"quantity"`
	Subtotal    int       `json:"subtotal"`
	CreatedAt   time.Time `json:"createdAt"`
}

// OrderGroup は一覧取得時に注文ごとに集約するための構造体
type OrderGroup struct {
	OrderNo     string      `json:"orderNo"`
	TerminalNo  string      `json:"terminalNo"`
	OrderStatus string      `json:"orderStatus"`
	TotalAmount int         `json:"totalAmount"`
	CreatedAt   time.Time   `json:"createdAt"`
	Items       []OrderItem `json:"items,omitempty"`
}

func initDB() {
	var err error
	// 同時書き込み対策のタイムアウト付与
	db, err = sql.Open("sqlite3", "order.db?_busy_timeout=5000")
	if err != nil {
		logger.Fatalf("[ERROR] DB接続に失敗しました: %v", err)
	}

	// 同時書き込み制限
	db.SetMaxOpenConns(1)

	schema := `
	CREATE TABLE IF NOT EXISTS order_items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		order_no TEXT NOT NULL,
		terminal_no TEXT NOT NULL,
		order_status TEXT NOT NULL,
		item_no INTEGER NOT NULL,
		menu_name TEXT NOT NULL,
		unit_price INTEGER NOT NULL,
		quantity INTEGER NOT NULL,
		subtotal INTEGER NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`

	if _, err := db.Exec(schema); err != nil {
		logger.Fatalf("[ERROR] テーブル作成に失敗しました: %v", err)
	}
	logger.Println("[INFO] データベース初期化完了 (order.db)")
}

// 採番ルール：MMDD-NNN (同一トランザクション内で実行)
func generateOrderNoAndInsert(terminalNo string, items []OrderItem) (string, error) {
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	now := time.Now()
	dateStr := now.Format("0102") // MMDD形式
	datePattern := dateStr + "-%"

	// 同日の最大連番を取得 (ロックを取得しつつ重複を防ぐ)
	var maxOrderNo sql.NullString
	query := `SELECT order_no FROM order_items WHERE order_no LIKE ? ORDER BY order_no DESC LIMIT 1`
	err = tx.QueryRow(query, datePattern).Scan(&maxOrderNo)
	
	nextSeq := 1
	if err == nil && maxOrderNo.Valid && len(maxOrderNo.String) == 8 {
		// MMDD-NNN の NNN 部分をパース
		var seq int
		_, errParse := fmt.Sscanf(maxOrderNo.String[5:], "%d", &seq)
		if errParse == nil {
			nextSeq = seq + 1
		}
	}

	orderNo := fmt.Sprintf("%s-%03d", dateStr, nextSeq)

	// 明細のINSERT
	stmt, err := tx.Prepare(`
		INSERT INTO order_items (order_no, terminal_no, order_status, item_no, menu_name, unit_price, quantity, subtotal)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return "", err
	}
	defer stmt.Close()

	for i, item := range items {
		logger.Printf("[DB登録内容] OrderNo: %s, ItemNo: %d, Menu: %s, Subtotal: %d", orderNo, i+1, item.MenuName, item.Subtotal)
		_, err = stmt.Exec(orderNo, terminalNo, StatusReceived, i+1, item.MenuName, item.UnitPrice, item.Quantity, item.Subtotal)
		if err != nil {
			return "", err
		}
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}

	return orderNo, nil
}

// 注文一覧をステータス指定（任意）で取得して集約
func getAggregatedOrders(statusFilter string) ([]OrderGroup, error) {
	var query string
	var args []interface{}

	if statusFilter != "" {
		query = `SELECT id, order_no, terminal_no, order_status, item_no, menu_name, unit_price, quantity, subtotal, created_at 
		         FROM order_items WHERE order_status = ? ORDER BY order_no ASC, item_no ASC`
		args = append(args, statusFilter)
	} else {
		query = `SELECT id, order_no, terminal_no, order_status, item_no, menu_name, unit_price, quantity, subtotal, created_at 
		         FROM order_items ORDER BY order_no ASC, item_no ASC`
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	orderMap := make(map[string]*OrderGroup)
	var orderOrder []string // 表示順を保持

	for rows.Next() {
		var item OrderItem
		var createdAtStr string
		err := rows.Scan(&item.ID, &item.OrderNo, &item.TerminalNo, &item.OrderStatus, &item.ItemNo, &item.MenuName, &item.UnitPrice, &item.Quantity, &item.Subtotal, &createdAtStr)
		if err != nil {
			return nil, err
		}
		
		// SQLiteの文字列時間をTimeに変換
		t, err := time.Parse("2006-01-02 15:04:05", createdAtStr)
		if err == nil {
			item.CreatedAt = t
		} else {
			// fallback
			item.CreatedAt = time.Now()
		}

		if _, exists := orderMap[item.OrderNo]; !exists {
			orderMap[item.OrderNo] = &OrderGroup{
				OrderNo:     item.OrderNo,
				TerminalNo:  item.TerminalNo,
				OrderStatus: item.OrderStatus,
				CreatedAt:   item.CreatedAt,
				Items:       []OrderItem{},
			}
			orderOrder = append(orderOrder, item.OrderNo)
		}
		orderMap[item.OrderNo].TotalAmount += item.Subtotal
		orderMap[item.OrderNo].Items = append(orderMap[item.OrderNo].Items, item)
	}

	result := []OrderGroup{}
	for _, no := range orderOrder {
		result = append(result, *orderMap[no])
	}
	return result, nil
}

// 特定の注文詳細を取得
func getOrderDetails(orderNo string) ([]OrderItem, error) {
	query := `SELECT id, order_no, terminal_no, order_status, item_no, menu_name, unit_price, quantity, subtotal, created_at 
	          FROM order_items WHERE order_no = ? ORDER BY item_no ASC`
	rows, err := db.Query(query, orderNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []OrderItem
	for rows.Next() {
		var item OrderItem
		var createdAtStr string
		err := rows.Scan(&item.ID, &item.OrderNo, &item.TerminalNo, &item.OrderStatus, &item.ItemNo, &item.MenuName, &item.UnitPrice, &item.Quantity, &item.Subtotal, &createdAtStr)
		if err != nil {
			return nil, err
		}
		t, _ := time.Parse("2006-01-02 15:04:05", createdAtStr)
		item.CreatedAt = t
		items = append(items, item)
	}
	return items, nil
}

// 注文ステータスを直接更新
func updateOrderStatus(orderNo string, nextStatus string) (int64, error) {
	logger.Printf("[DB更新内容] OrderNo: %s を ステータス: %s に更新を試みます", orderNo, nextStatus)
	res, err := db.Exec(`UPDATE order_items SET order_status = ? WHERE order_no = ?`, nextStatus, orderNo)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// 掲示板用データ取得
func getBoardOrders() ([]string, []string, error) {
	query := `SELECT order_no, order_status FROM order_items GROUP BY order_no`
	rows, err := db.Query(query)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var cooking []string
	var ready []string

	for rows.Next() {
		var orderNo, status string
		if err := rows.Scan(&orderNo, &status); err != nil {
			return nil, nil, err
		}
		if status == StatusReceived {
			cooking = append(cooking, orderNo)
		} else if status == StatusCooked {
			ready = append(ready, orderNo)
		}
	}
	return cooking, ready, nil
}

// 厨房用未調理データ取得
func getKitchenOrders() ([]map[string]interface{}, error) {
	orders, err := getAggregatedOrders(StatusReceived)
	if err != nil {
		return nil, err
	}

	var result []map[string]interface{}
	for _, o := range orders {
		var itemsList []map[string]interface{}
		for _, item := range o.Items {
			itemsList = append(itemsList, map[string]interface{}{
				"menuName": item.MenuName,
				"quantity": item.Quantity,
			})
		}
		result = append(result, map[string]interface{}{
			"orderNo": o.OrderNo,
			"items":   itemsList,
		})
	}
	return result, nil
}