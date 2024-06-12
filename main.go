package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v2"
)

type Config struct {
	Symbols    []string `yaml:"symbols"`
	MaxWorkers int      `yaml:"max_workers"`
}

type PriceMessage struct {
	Symbol  string
	Price   float64
	Changed bool
}

type Worker struct {
	symbols      []string
	lastPrices   map[string]float64 // я бы вынес логику отслеживания исзменений отсюда на слой выше и централизованно следил бы, а воркер сделал бы максимально простым
	requestCount int64              // надо закрывать атомиком или мьютексом, потому что ты обращаешься к этой переменной из разных горутин (еще в маин читаешь)
	outputCh     chan PriceMessage
}

type TickerResponse struct {
	Symbol string `json:"symbol"`
	Price  string `json:"price"` // можно использовать json.Number  и не писать кастомный парсер из строки в число
}

func (w *Worker) Run(wg *sync.WaitGroup, stopCh chan struct{}) {
	defer wg.Done()
	w.lastPrices = make(map[string]float64) // лучше выносить такое в консруктор, если случайно вызовем run несколько раз, будут проблемы
	for {
		select {
		case <-stopCh:
			return
		default:
			for _, symbol := range w.symbols { // думаю лучше ставить селект внутри этого цикла - в твоем случае, когда мы отправим стоп сигнал, придется ждать когда все символы из списка отработают
				price, err := fetchPrice(symbol)
				if err != nil {
					log.Println(err)
					continue // забыл увеличить счетчик запросов
				}
				previousPrice, exists := w.lastPrices[symbol]

				if !exists || previousPrice == price {
					w.outputCh <- PriceMessage{Symbol: symbol, Price: price, Changed: false}

				} else {
					w.outputCh <- PriceMessage{Symbol: symbol, Price: price, Changed: true}
				}
				w.lastPrices[symbol] = price
				// тут можно было бы использовать atomic.AddInt64, но так как только одна горутина меняет это, то можно и так
				w.requestCount++
			}

		}
	}
}

func (w *Worker) GetRequestsCount() int {
	return int(w.requestCount)
}

func readConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err // хорошая практика использовать errors.Wrap
	}
	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

func fetchPrice(symbol string) (float64, error) {
	url := fmt.Sprintf("https://api.binance.com/api/v3/ticker/price?symbol=%s", symbol)
	response, err := http.Get(url) // лучше не использовать DefaultClient - у него нет таймаута, если что-то пойдет не так, зависнешь навсегда тут
	if err != nil {
		return 0, fmt.Errorf("error making request to Binance API: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("received non-200 status code: %d", response.StatusCode)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, fmt.Errorf("error reading response body: %v", err)
	}

	var ticker TickerResponse
	if err := json.Unmarshal(body, &ticker); err != nil {
		return 0, fmt.Errorf("error unmarshalling JSON response: %v", err)
	}

	price, err := parsePrice(ticker.Price)
	if err != nil {
		return 0, fmt.Errorf("error parsing price: %v", err)
	}

	return price, nil
}

// parsePrice - слишком кастомное название, мы же можем парсить любые строки (не только цену) - я бы назвал strToFloat64 и смог переиспользовать в любых похожих кейсах
func parsePrice(priceStr string) (float64, error) {
	var price float64
	if _, err := fmt.Sscanf(priceStr, "%f", &price); err != nil {
		return 0, fmt.Errorf("error parsing price string: %v", err)
	}
	return price, nil
}

func main() {
	scanner := bufio.NewReader(os.Stdin)
	fmt.Print("Enter command (START to begin, STOP to end): ")
	cmd, _ := scanner.ReadString('\n')
	cmd = strings.TrimSpace(cmd)
	if cmd != "START" { // YAGNY - в задаче не было такого требования
		log.Println("Command to start not received, exiting...")
		return
	}

	config, err := readConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to read config: %v", err)
	}
	if config.MaxWorkers <= 0 { // я бы вынес эту проверку выше - зачем заствлять пользователя что-то вводить, а потом писать ошибку, что у него конфига не валидна (вообще лучше использовать пакет validator и валидировать сразу конфигу)
		log.Fatalf("Invalid max_workers value: %d", config.MaxWorkers)
	}
	coreCount := runtime.NumCPU()
	if config.MaxWorkers > coreCount {
		config.MaxWorkers = coreCount
	}
	// так как синхронизация не требуется, то можно использовать буферизированный канал
	outputCh := make(chan PriceMessage, 100)
	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	var workers []*Worker
	symbolsPerWorker := len(config.Symbols) / config.MaxWorkers
	extraSymbols := len(config.Symbols) % config.MaxWorkers

	startIndex := 0
	// Каждому воркеру даем часть символов
	for i := 0; i < config.MaxWorkers; i++ { // я бы вынес разбивку символов на группы и потом уже создавал для них воркеры (так ты как минимум сможешь отдельно протестировать группировку)
		endIndex := startIndex + symbolsPerWorker
		if i < extraSymbols {
			endIndex++
		}

		if startIndex == endIndex {
			continue
		}

		worker := &Worker{
			symbols:  config.Symbols[startIndex:endIndex],
			outputCh: outputCh,
		}
		workers = append(workers, worker)
		startIndex = endIndex

		wg.Add(1)
		go worker.Run(&wg, stopCh) // в общем я бы разделил логику создания и подготовки и логику запуска воркеров - это было бы читабельнее
	}

	var wg2 sync.WaitGroup // не вижу смысла 2 разных wg делать - можно переиспользовать одну, ты же ждешь 1 раз в маин
	// это чтобы читать сообщения из канала и выводить их в консоль
	wg2.Add(1)
	go func() {
		defer wg2.Done()
		i := 1
		for msg := range outputCh {
			time.Sleep(100 * time.Millisecond) // не понял зачем тут ожидание
			fmt.Printf("Message %d\n", i)      // YAGNY
			i++
			if msg.Changed {
				fmt.Printf("%s price:%.2f changed\n", msg.Symbol, msg.Price)
			} else {
				fmt.Printf("%s price:%.2f\n", msg.Symbol, msg.Price)
			}
		}
	}()

	go func() {
		for range time.Tick(5 * time.Second) {
			var totalRequests int
			for _, w := range workers {
				totalRequests += w.GetRequestsCount()
			}
			fmt.Printf("\nworkers requests total: %d\n\nclear", totalRequests)
		}
	}()

	for { // я бы все же wg.Wait() вынес из цикла  - а работу с инпутом запустил бы в горутине
		cmd, _ := scanner.ReadString('\n') // ошибки лучше всегда обрабатывать
		if strings.TrimSpace(cmd) == "STOP" {
			// даем команду "остановить" всем воркерам
			close(stopCh) // так норм, но еще лучше использовать контекст с отменой
			wg.Wait()     // ждем воркеров
			close(outputCh)
			wg2.Wait() // ждем пока все сообщения выведутся
			break
		}
	}
}
