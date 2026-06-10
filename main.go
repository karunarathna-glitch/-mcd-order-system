package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var logger *log.Logger

func initLogger() func() {
	logDir := "logs"
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Fatalf("ログディレクトリの作成に失敗しました: %v", err)
	}

	logFile, err := os.OpenFile(filepath.Join(logDir, "order.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("ログファイルのオープンに失敗しました: %v", err)
	}

	// 標準出力とファイルの両方に出力
	mw := io.MultiWriter(os.Stdout, logFile)
	logger = log.New(mw, "", log.LstdFlags|log.Lmicroseconds)

	return func() {
		logFile.Close()
	}
}

func main() {
	closeLog := initLogger()
	defer closeLog()

	logger.Println("[INFO] アプリケーションを起動しています...")

	// データベース初期化
	initDB()
	defer db.Close()

	// Go 1.22+ の新しいServeMux機能を利用したルーティング定義
	mux := http.NewServeMux()

	// CORSミドルウェアを適用してハンドラを登録
	mux.Handle("POST /api/orders", corsMiddleware(http.HandlerFunc(handlePostOrders)))
	mux.Handle("GET /api/orders", corsMiddleware(http.HandlerFunc(handleGetOrders)))
	mux.Handle("GET /api/orders/{orderNo}", corsMiddleware(http.HandlerFunc(handleGetOrderByNo)))
	mux.Handle("PUT /api/orders/{orderNo}/status", corsMiddleware(http.HandlerFunc(handlePutOrderStatus)))
	mux.Handle("POST /api/board", corsMiddleware(http.HandlerFunc(handlePostBoard)))
	mux.Handle("POST /api/kitchen", corsMiddleware(http.HandlerFunc(handlePostKitchen)))

	srv := &http.Server{
		Addr:    "0.0.0.0:8080",
		Handler: mux,
	}

	// グレースフルシャットダウンの準備
	go func() {
		logger.Printf("[INFO] サーバーが 0.0.0.0:8080 で起動しました。")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("[ERROR] サーバー起動に失敗しました: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Println("[INFO] サーバーを停止しています...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Fatalf("[ERROR] サーバーの強制停止: %v", err)
	}
	logger.Println("[INFO] サーバーが正常に終了しました。")
}