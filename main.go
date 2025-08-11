package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lmittmann/tint"

	unstruct "github.com/vivaneiona/genkit-unstruct"
	"github.com/vivaneiona/gonfig"
	"google.golang.org/genai"
	"gopkg.in/telebot.v3"
	_ "modernc.org/sqlite"
)

type Config struct {
	BotToken     string `secret:"TELEGRAM_BOT_TOKEN"`
	GeminiAPIKey string `secret:"GEMINI_API_KEY"`
	DBURL        string `env:"DATABASE_URL" default:"file:spends.db?_fk=1"`
}

type Position struct {
	Name  string  `json:"name"`
	Price float64 `json:"price"`
}

type SpendExtraction struct {
	Currency    string     `json:"currency" unstruct:"prompt/currency/model/gemini-1.5-flash?temperature=0.0&topK=1"`
	Spend       float64    `json:"spend"    unstruct:"prompt/receipt/model/gemini-2.5-pro?temperature=0.0&topK=1"`
	Positions   []Position `json:"positions" unstruct:"prompt/receipt/model/gemini-2.5-pro?temperature=0.0&topK=1"`
	CashierName string     `json:"cachier"  unstruct:"prompt/cachier/model/gemini-1.5-flash?temperature=0.0&topK=1"`
}

type SpendRecord struct {
	ID          string
	TgUserID    int64
	CreatedAt   time.Time
	Source      string
	RawText     string
	SpendTotal  float64
	Currency    string
	ItemName    string
	ItemPrice   float64
	CashierName string
	JSON        string
}

func main() {
	ctx := context.Background()

	logger := slog.New(tint.NewHandler(os.Stderr, &tint.Options{
		Level:      slog.LevelDebug,
		TimeFormat: time.Kitchen,
	}))
	slog.SetDefault(logger)

	cfg, err := gonfig.LoadWithDotenv(Config{})

	if err != nil {
		logger.Error("config load", "err", err)
		os.Exit(1)
	}
	if cfg.BotToken == "" || cfg.GeminiAPIKey == "" {
		logger.Error("missing TELEGRAM_BOT_TOKEN or GEMINI_API_KEY")
		os.Exit(1)
	}

	db, err := sql.Open("sqlite", cfg.DBURL)
	if err != nil {
		logger.Error("db open", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := initDB(db); err != nil {
		logger.Error("db init", "err", err)
		os.Exit(1)
	}

	// 1️⃣ Genkit initialization
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Backend: genai.BackendGeminiAPI,
		APIKey:  cfg.GeminiAPIKey,
	})

	if err != nil {
		logger.Error("genai client", "err", err)
		os.Exit(1)
	}

	// 2️⃣ Prompts provider
	prompts, err := unstruct.NewStickPromptProvider(unstruct.WithFS(os.DirFS("."), "templates"))
	if err != nil {
		logger.Error("prompt provider", "err", err)
		os.Exit(1)
	}

	// 3️⃣ Unstruct initialization
	extractor := unstruct.NewWithLogger[SpendExtraction](client, prompts, logger)

	// 4️⃣ Telegram bot initialization
	b, err := telebot.NewBot(telebot.Settings{
		Token:  cfg.BotToken,
		Poller: &telebot.LongPoller{Timeout: 10 * time.Second},
	})

	if err != nil {
		logger.Error("bot create", "err", err)
		os.Exit(1)
	}

	b.Handle("/start", func(c telebot.Context) error {
		return c.Send(`Send a receipt photo or text like: "Bought milk for 100 THB".`)
	})

	b.Handle(telebot.OnText, func(c telebot.Context) error {
		return extractAndPersist(
			ctx, c, extractor, db, logger,
			"text", c.Text(),
		)
	})

	b.Handle(telebot.OnPhoto, func(c telebot.Context) error {
		if c.Message().Photo == nil {
			return c.Send("No photo found.")
		}
		return extractAndPersist(ctx, c, extractor, db, logger, "photo", "")
	})

	b.Handle(telebot.OnVoice, func(c telebot.Context) error {
		if c.Message().Voice == nil {
			return c.Send("No voice message found.")
		}
		return extractAndPersist(ctx, c, extractor, db, logger, "voice", "")
	})

	b.Handle(telebot.OnAudio, func(c telebot.Context) error {
		if c.Message().Audio == nil {
			return c.Send("No audio found.")
		}
		return extractAndPersist(ctx, c, extractor, db, logger, "audio", "")
	})

	logger.Info("bot started")
	b.Start()
}

// Common pipeline
func extractAndPersist(
	ctx context.Context,
	c telebot.Context,
	extractor *unstruct.Unstructor[SpendExtraction],
	db *sql.DB,
	logger *slog.Logger,
	source string,
	rawText string,
) error {
	// 4️⃣ Build assets (voices, images, videos, texts) for extraction
	assets, err := buildAssets(c, source, rawText)
	if err != nil {
		return c.Send(err.Error())
	}

	// 5️⃣ Run unstructor
	res, err := extractor.Unstruct(ctx, assets)
	if err != nil {
		logger.Warn("extract", "err", err)
		return c.Send(fmt.Sprintf("Failed to process: %v", err))
	}

	records, total := buildRecords(c.Sender().ID, source, rawText, res)

	// 🥂 Persist the result
	if err := saveMultipleSpends(db, records); err != nil {
		logger.Warn("save", "err", err)
		return c.Send(fmt.Sprintf("Failed to save: %v", err))
	}

	return replySummary(c, res, total)
}

func buildAssets(c telebot.Context, source, rawText string) ([]unstruct.Asset, error) {
	switch source {
	case "text":
		return []unstruct.Asset{unstruct.NewTextAsset(rawText)}, nil
	case "photo":
		p := c.Message().Photo
		if p == nil {
			return nil, fmt.Errorf("No photo found.")
		}
		data, err := readTelegramFile(c, &p.File)
		if err != nil {
			return nil, fmt.Errorf("Failed to read photo.")
		}
		return []unstruct.Asset{unstruct.NewDataAsset(data, "image/jpeg")}, nil
	case "voice":
		v := c.Message().Voice
		if v == nil {
			return nil, fmt.Errorf("No voice message found.")
		}
		data, err := readTelegramFile(c, &v.File)
		if err != nil {
			return nil, fmt.Errorf("Failed to read voice message.")
		}
		return []unstruct.Asset{unstruct.NewDataAsset(data, "audio/ogg")}, nil
	case "audio":
		a := c.Message().Audio
		if a == nil {
			return nil, fmt.Errorf("No audio found.")
		}
		data, err := readTelegramFile(c, &a.File)
		if err != nil {
			return nil, fmt.Errorf("Failed to read audio.")
		}
		return []unstruct.Asset{unstruct.NewDataAsset(data, "audio/mpeg")}, nil
	default:
		return nil, fmt.Errorf("Unsupported source.")
	}
}

func readTelegramFile(c telebot.Context, f *telebot.File) ([]byte, error) {
	r, err := c.Bot().File(f)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// Records + reply
func buildRecords(tgID int64, source, rawText string, res *SpendExtraction) ([]SpendRecord, float64) {
	baseID := uuid.New().String()
	total := res.Spend
	if total == 0 {
		for _, p := range res.Positions {
			total += p.Price
		}
	}

	jsonData, _ := json.Marshal(res)
	ts := time.Now()

	if len(res.Positions) == 0 {
		return []SpendRecord{{
			ID:          baseID,
			TgUserID:    tgID,
			CreatedAt:   ts,
			Source:      source,
			RawText:     rawText,
			SpendTotal:  total,
			Currency:    res.Currency,
			ItemName:    "Unknown item",
			ItemPrice:   total,
			CashierName: res.CashierName,
			JSON:        string(jsonData),
		}}, total
	}

	out := make([]SpendRecord, 0, len(res.Positions))
	for i, p := range res.Positions {
		out = append(out, SpendRecord{
			ID:          fmt.Sprintf("%s-%d", baseID, i),
			TgUserID:    tgID,
			CreatedAt:   ts,
			Source:      source,
			RawText:     rawText,
			SpendTotal:  total,
			Currency:    res.Currency,
			ItemName:    p.Name,
			ItemPrice:   p.Price,
			CashierName: res.CashierName,
			JSON:        string(jsonData),
		})
	}
	return out, total
}

func replySummary(c telebot.Context, res *SpendExtraction, total float64) error {
	cashier := ""
	if res.CashierName != "" {
		cashier = fmt.Sprintf(" | Cashier <code>%s</code>", res.CashierName)
	}

	mode := &telebot.SendOptions{ParseMode: telebot.ModeHTML}
	items := make([]string, 0, len(res.Positions))
	for _, p := range res.Positions {
		items = append(items, fmt.Sprintf("<b>%s</b> (<i>%.2f</i> %s)", p.Name, p.Price, res.Currency))
	}
	return c.Send(fmt.Sprintf("<b>TOTAL</b> %.2f %s %s:\n%s",
		total, res.Currency, cashier, strings.Join(items, "\n")), mode)
}

func initDB(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS spends (
	id           TEXT PRIMARY KEY,
	tg_user_id   INTEGER NOT NULL,
	created_at   TIMESTAMP NOT NULL,
	source       TEXT NOT NULL,
	raw_text     TEXT,
	spend_total  REAL NOT NULL,
	currency     TEXT,
	item_name    TEXT,
	item_price   REAL,
	cashier_name TEXT,
	json         TEXT
);
`)
	return err
}

func saveMultipleSpends(db *sql.DB, records []SpendRecord) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`
INSERT INTO spends (
	id, tg_user_id, created_at, source, raw_text,
	spend_total, currency, item_name, item_price, cashier_name, json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, r := range records {
		if _, err := stmt.Exec(
			r.ID, r.TgUserID, r.CreatedAt, r.Source, r.RawText,
			r.SpendTotal, r.Currency, r.ItemName, r.ItemPrice, r.CashierName, r.JSON,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
