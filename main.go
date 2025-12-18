package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	_ "github.com/go-sql-driver/mysql"
)

const dbConnString = "root@tcp(localhost:3306)/lesha?parseTime=true"

var db *sql.DB

func main() {
	var err error
	db, err = sql.Open("mysql", dbConnString)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err = db.Ping(); err != nil {
		log.Fatal("Cannot connect to DB:", err)
	}

	http.Handle("/", http.FileServer(http.Dir("./static")))

	http.HandleFunc("/api/failures", getFailuresHandler)
	http.HandleFunc("/api/order", createOrderHandler)
	http.HandleFunc("/api/defects", updateDefectsHandler)
	http.HandleFunc("/api/polishing", checkPolishingHandler)
	http.HandleFunc("/api/options", getOptionsHandler)

	fmt.Println("Server running on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// --- УМНАЯ ЗАМЕНА (RegExp) ---
// Находит "оборудование X", "оборудования X" и т.д. и меняет на название
func enrichLogMessage(msg string) string {
	if msg == "" {
		return ""
	}

	// 1. Получаем карту всех ID -> Название
	rows, err := db.Query("SELECT equipment_id, title FROM equipment")
	if err != nil {
		return msg
	}
	defer rows.Close()

	equipMap := make(map[string]string)
	for rows.Next() {
		var id int
		var title string
		if err := rows.Scan(&id, &title); err == nil {
			equipMap[strconv.Itoa(id)] = title
		}
	}

	// 2. Регулярное выражение:
	// (?i) - регистронезависимо
	// (оборудовани[а-я]*) - ловит "оборудование", "оборудования" и т.д.
	// \s+ - пробелы
	// (\d+) - ловит ID
	re := regexp.MustCompile(`(?i)(оборудовани[а-я]*)\s+(\d+)`)

	// 3. Замена
	return re.ReplaceAllStringFunc(msg, func(match string) string {
		// match = "оборудования 1"
		submatches := re.FindStringSubmatch(match)
		if len(submatches) < 3 {
			return match
		}
		word := submatches[1] // "оборудования" (сохраняем падеж, если хотим, или просто слово)
		id := submatches[2]   // "1"

		if name, ok := equipMap[id]; ok {
			// Возвращаем: "оборудования Фильтр (ID 1)"
			return fmt.Sprintf("%s \"%s\" (ID %s)", word, name, id)
		}
		return match
	})
}

// --- Задача 1 ---
type Failure struct {
	Equipment   string  `json:"equipment"`
	Process     string  `json:"process"`
	Reason      string  `json:"reason"`
	FailureDate string  `json:"failure_date"`
	RestoreDate *string `json:"restore_date"`
}

func getFailuresHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`
		SELECT e.title, ef.process_name, ef.failure_reason, ef.failure_date, ef.restore_date 
		FROM equipment_failures ef
		JOIN equipment e ON ef.equipment_id = e.equipment_id
		ORDER BY ef.failure_date DESC
	`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	var failures []Failure
	for rows.Next() {
		var f Failure
		rows.Scan(&f.Equipment, &f.Process, &f.Reason, &f.FailureDate, &f.RestoreDate)
		failures = append(failures, f)
	}
	json.NewEncoder(w).Encode(failures)
}

// --- Задача 2 ---
type OrderRequest struct {
	LensName    string  `json:"lensName"`
	OptPower    float64 `json:"optPower"`
	BaseCurve   float64 `json:"baseCurve"`
	Diameter    float64 `json:"diameter"`
	Thickness   float64 `json:"thickness"`
	EquipID     int     `json:"equipID"`
	Speed       float64 `json:"speed"`
	Pressure    float64 `json:"pressure"`
	Temperature float64 `json:"temperature"`
}
type OrderResponse struct {
	Message         string `json:"message"`
	PredictedOutput int    `json:"predicted_output"`
	Status          string `json:"status"`
}

func createOrderHandler(w http.ResponseWriter, r *http.Request) {
	var req OrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", 400)
		return
	}
	_, err := db.Exec(`CALL process_lens_order(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.LensName, req.OptPower, req.BaseCurve, req.Diameter, req.Thickness,
		req.EquipID, req.Speed, req.Pressure, req.Temperature)

	if err != nil {
		// Ошибки тоже прогоняем через улучшатель текста
		cleanErr := enrichLogMessage(err.Error())
		// Убираем технический код SQL (Error 1644: ...)
		if idx := strings.Index(cleanErr, ": "); idx != -1 {
			cleanErr = cleanErr[idx+2:]
		}
		json.NewEncoder(w).Encode(OrderResponse{Status: "error", Message: cleanErr})
		return
	}
	var predicted int
	_ = db.QueryRow("SELECT calc_output(?, ?, ?)", req.Speed, req.Pressure, req.Temperature).Scan(&predicted)
	json.NewEncoder(w).Encode(OrderResponse{
		Status:          "success",
		Message:         "Параметры приняты. Нормы соблюдены.",
		PredictedOutput: predicted,
	})
}

// --- Задача 3 ---
type DefectRequest struct {
	ProductID int `json:"product_id"`
	NewValue  int `json:"new_value"`
}
type DefectResponse struct {
	Recommendation string `json:"recommendation"`
	ActionRequired bool   `json:"action_required"`
}

func updateDefectsHandler(w http.ResponseWriter, r *http.Request) {
	var req DefectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", 400)
		return
	}
	_, err := db.Exec("UPDATE number_of_defects SET value = ? WHERE product_id = ?", req.NewValue, req.ProductID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var logMsg string
	err = db.QueryRow(`SELECT log_message FROM production_logs WHERE product_id = ? ORDER BY log_time DESC LIMIT 1`, req.ProductID).Scan(&logMsg)

	if err != nil {
		json.NewEncoder(w).Encode(DefectResponse{Recommendation: "В пределах нормы.", ActionRequired: false})
		return
	}

	// !!! ВОТ ЗДЕСЬ МАГИЯ ЗАМЕНЫ ИМЕН !!!
	rec := enrichLogMessage(logMsg)

	if strings.Contains(logMsg, "OK") || strings.Contains(logMsg, "снижено") {
		rec = "Параметры скорректированы автоматически или находятся в норме."
	}
	json.NewEncoder(w).Encode(DefectResponse{Recommendation: rec, ActionRequired: true})
}

// --- Задача 4 ---
func checkPolishingHandler(w http.ResponseWriter, r *http.Request) {
	productID := 1
	equipID := 7
	_, err := db.Exec("CALL check_equipment_parameters(?, ?)", equipID, productID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var logMsg string
	err = db.QueryRow(`SELECT log_message FROM production_logs WHERE product_id = ? AND log_message LIKE 'Оборудование 7%' ORDER BY log_time DESC LIMIT 1`, productID).Scan(&logMsg)

	status := "Параметры полировки в норме."
	isCritical := false
	if err == nil && logMsg != "" {
		status = "ВНИМАНИЕ: " + enrichLogMessage(logMsg)
		isCritical = true
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "critical": isCritical})
}

// --- Options ---
func getOptionsHandler(w http.ResponseWriter, r *http.Request) {
	pRows, _ := db.Query("SELECT product_id, title, batch_number FROM finished_products")
	defer pRows.Close()
	type ProductOpt struct {
		ID    int    `json:"id"`
		Label string `json:"label"`
	}
	var products []ProductOpt
	for pRows.Next() {
		var id int
		var title, batch string
		pRows.Scan(&id, &title, &batch)
		products = append(products, ProductOpt{ID: id, Label: fmt.Sprintf("%s (Партия: %s)", title, batch)})
	}
	eRows, _ := db.Query("SELECT equipment_id, title FROM equipment")
	defer eRows.Close()
	type EquipOpt struct {
		ID    int    `json:"id"`
		Label string `json:"label"`
	}
	var equipments []EquipOpt
	for eRows.Next() {
		var id int
		var title string
		eRows.Scan(&id, &title)
		equipments = append(equipments, EquipOpt{ID: id, Label: title})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"products": products, "equipment": equipments})
}
