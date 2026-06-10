package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// CORS共通ミドルウェア
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// JSONエラーレスポンスヘルパー
func respondWithError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp, _ := json.Marshal(map[string]string{"result": "NG", "error": message})
	logger.Printf("[API出電文] エラー応答 (Code: %d): %s", code, string(resp))
	w.Write(resp)
}

// JSON成功レスポンスヘルパー
func respondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp, _ := json.Marshal(payload)
	logger.Printf("[API出電文] 正常応答 (Code: %d): %s", code, string(resp))
	w.Write(resp)
}

// 3.1 POST /api/orders
func handlePostOrders(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MessageType string `json:"messageType"`
		TerminalNo  string `json:"terminalNo"`
		TotalAmount int    `json:"totalAmount"`
		Items       []struct {
			MenuName  string `json:"menuName"`
			UnitPrice int    `json:"unitPrice"`
			Quantity  int    `json:"quantity"`
			Subtotal  int    `json:"subtotal"`
		} `json:"items"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "不正なJSON形式です")
		return
	}

	reqBytes, _ := json.Marshal(req)
	logger.Printf("[API入電文] POST /api/orders: %s", string(reqBytes))

	// 単一入力チェック要件の検証
	if req.TerminalNo == "" {
		respondWithError(w, http.StatusBadRequest, "terminalNo は必須です")
		return
	}
	if req.MessageType != "ORDER_CONFIRM" {
		respondWithError(w, http.StatusBadRequest, "messageType は ORDER_CONFIRM である必要があります")
		return
	}
	if req.TotalAmount < 1 {
		respondWithError(w, http.StatusBadRequest, "totalAmount は1以上である必要があります")
		return
	}
	if len(req.Items) < 1 || len(req.Items) > 5 {
		respondWithError(w, http.StatusBadRequest, "明細アイテム数は1〜5件である必要があります")
		return
	}

	calculatedTotal := 0
	menuSet := make(map[string]bool)
	var dbItems []OrderItem

	for _, item := range req.Items {
		if item.MenuName == "" {
			respondWithError(w, http.StatusBadRequest, "menuName は必須です")
			return
		}
		if item.UnitPrice < 1 {
			respondWithError(w, http.StatusBadRequest, "unitPrice は1以上である必要があります")
			return
		}
		if item.Quantity < 1 || item.Quantity > 5 {
			respondWithError(w, http.StatusBadRequest, "quantity は1〜5の範囲内である必要があります")
			return
		}
		if menuSet[item.MenuName] {
			respondWithError(w, http.StatusBadRequest, "同一注文内で menuName の重複は禁止されています")
			return
		}
		menuSet[item.MenuName] = true

		calcSubtotal := item.UnitPrice * item.Quantity
		if item.Subtotal != calcSubtotal {
			respondWithError(w, http.StatusBadRequest, "明細の subtotal 計算が正しくありません (単価×数量不一致)")
			return
		}
		calculatedTotal += calcSubtotal

		dbItems = append(dbItems, OrderItem{
			MenuName:  item.MenuName,
			UnitPrice: item.UnitPrice,
			Quantity:  item.Quantity,
			Subtotal:  item.Subtotal,
		})
	}

	if calculatedTotal != req.TotalAmount {
		respondWithError(w, http.StatusBadRequest, "明細の小計合計が totalAmount と一致しません")
		return
	}

	// 採番と登録を単一トランザクションで実行
	orderNo, err := generateOrderNoAndInsert(req.TerminalNo, dbItems)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "注文の登録に失敗しました: "+err.Error())
		return
	}

	respondWithJSON(w, http.StatusCreated, map[string]interface{}{
		"result":      "OK",
		"orderNo":     orderNo,
		"orderStatus": StatusReceived,
		"totalAmount": req.TotalAmount,
		"message":     "オーダを受信しました。",
	})
}

// 3.1 GET /api/orders (一覧 & 状態別一覧)
func handleGetOrders(w http.ResponseWriter, r *http.Request) {
	logger.Printf("[API入電文] GET /api/orders?%s", r.URL.RawQuery)
	statusFilter := r.URL.Query().Get("status")

	orders, err := getAggregatedOrders(statusFilter)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondWithJSON(w, http.StatusOK, orders)
}

// 3.1 GET /api/orders/{orderNo}
func handleGetOrderByNo(w http.ResponseWriter, r *http.Request) {
	orderNo := r.PathValue("orderNo")
	logger.Printf("[API入電文] GET /api/orders/%s", orderNo)

	if orderNo == "" {
		respondWithError(w, http.StatusBadRequest, "orderNoが指定されていません")
		return
	}

	items, err := getOrderDetails(orderNo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if len(items) == 0 {
		respondWithError(w, http.StatusNotFound, "指定された注文番号が見つかりません")
		return
	}

	respondWithJSON(w, http.StatusOK, items)
}

// 3.1 PUT /api/orders/{orderNo}/status
func handlePutOrderStatus(w http.ResponseWriter, r *http.Request) {
	orderNo := r.PathValue("orderNo")
	var req struct {
		OrderStatus string `json:"orderStatus"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "不正なJSON形式です")
		return
	}

	reqBytes, _ := json.Marshal(req)
	logger.Printf("[API入電文] PUT /api/orders/%s/status: %s", orderNo, string(reqBytes))

	if req.OrderStatus != StatusReceived && req.OrderStatus != StatusCooked && req.OrderStatus != StatusDelivered {
		respondWithError(w, http.StatusBadRequest, "不正なステータスです")
		return
	}

	affected, err := updateOrderStatus(orderNo, req.OrderStatus)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if affected == 0 {
		respondWithError(w, http.StatusNotFound, "指定された注文番号が存在しません")
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{
		"result":  "OK",
		"message": "ステータスを更新しました",
	})
}

// 3.2 POST /api/board
func handlePostBoard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TerminalNo  string `json:"terminalNo"`
		MessageType string `json:"messageType"`
		OrderNo     string `json:"orderNo,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "不正なJSON形式です")
		return
	}

	reqBytes, _ := json.Marshal(req)
	logger.Printf("[API入電文] POST /api/board: %s", string(reqBytes))

	if req.MessageType != "BOARD_REQUEST" {
		respondWithError(w, http.StatusBadRequest, "messageType は BOARD_REQUEST である必要があります")
		return
	}

	// orderNoが指定されている場合は「受け渡し済み」に更新
	if strings.TrimSpace(req.OrderNo) != "" {
		_, err := updateOrderStatus(req.OrderNo, StatusDelivered)
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// 最新の掲示板情報を取得
	cooking, ready, err := getBoardOrders()
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 確実に配列の形(JSONで[]かnullではなく空配列)にする対策
	if cooking == nil {
		cooking = []string{}
	}
	if ready == nil {
		ready = []string{}
	}

	respondWithJSON(w, http.StatusOK, map[string]interface{}{
		"result":        "OK",
		"cookingOrders": cooking,
		"readyOrders":   ready,
	})
}

// 3.3 POST /api/kitchen
func handlePostKitchen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TerminalNo  string `json:"terminalNo,omitempty"`
		MessageType string `json:"messageType"`
		OrderNo     string `json:"orderNo,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "不正なJSON形式です")
		return
	}

	reqBytes, _ := json.Marshal(req)
	logger.Printf("[API入電文] POST /api/kitchen: %s", string(reqBytes))

	if req.MessageType != "KITCHEN_REQUEST" {
		respondWithError(w, http.StatusBadRequest, "messageType は KITCHEN_REQUEST である必要があります")
		return
	}

	// orderNoが指定されている場合は「調理済み」に更新
	if strings.TrimSpace(req.OrderNo) != "" {
		_, err := updateOrderStatus(req.OrderNo, StatusCooked)
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// 最新の未調理一覧を取得
	kitchenList, err := getKitchenOrders()
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if kitchenList == nil {
		kitchenList = []map[string]interface{}{}
	}

	respondWithJSON(w, http.StatusOK, map[string]interface{}{
		"result": "OK",
		"orders": kitchenList,
	})
}