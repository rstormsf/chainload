package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gochain-io/gochain/accounts/keystore"
	"github.com/gochain-io/gochain/ethclient"
	"github.com/pkg/errors"
)

type Config struct {
	id      uint64
	urlsCSV string
	tps     int
	senders int
	dur     time.Duration
	pass    string
	gas     uint64
	amount  *big.Int
	verbose bool
}

var config = Config{}

func init() {
	flag.Uint64Var(&config.id, "id", 1234, "id")
	flag.StringVar(&config.urlsCSV, "urls", "http://localhost:8545", "csv of urls")
	flag.IntVar(&config.tps, "tps", 1, "transactions per second")
	flag.IntVar(&config.senders, "senders", 0, "total number of concurrent senders/accounts - defaults to 1/10 of tps")
	flag.DurationVar(&config.dur, "dur", 0, "duration to run - omit for unlimited")
	flag.StringVar(&config.pass, "pass", "#go@chain42", "passphrase to unlock accounts")
	flag.Uint64Var(&config.gas, "gas", 200000, "gas")
	config.amount = new(big.Int).SetUint64(1)
	flag.BoolVar(&config.verbose, "v", false, "verbose logging")
}

func main() {
	// pprof
	runtime.SetBlockProfileRate(1000000)
	runtime.SetMutexProfileFraction(1000000)
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	nodes, err := setup()
	if err != nil {
		log.Fatalf("Failed to set up\terr=%q\n", err)
	}
	log.Printf("Starting execution\tid=%d urls=%s tps=%d senders=%d duration=%s gas=%d amount=%d\n",
		config.id, config.urlsCSV, config.tps, config.senders, config.dur, config.gas, config.amount)
	err = run(nodes)
	if err != nil {
		log.Fatalf("Failed\terr=%q\n", err)
	}
}

func setup() ([]*Node, error) {
	if fi, err := os.Stdin.Stat(); err != nil {
		return nil, err
	} else if fi.Size() > 0 {
		return nil, errors.New("illegal input: non-empty stdin")
	}
	flag.Parse()
	if len(flag.Args()) > 0 {
		return nil, fmt.Errorf("illegal extra arguments: %v", flag.Args())
	}
	if config.senders == 0 {
		config.senders = config.tps / 10
		if config.senders == 0 {
			config.senders = 1
		}
	}

	as := NewAccountStore(keystore.NewPlaintextKeyStore("keystore"), new(big.Int).SetUint64(config.id))
	urls := strings.Split(config.urlsCSV, ",")

	var nodes []*Node
	for i := range urls {
		url := urls[i]
		client, err := ethclient.Dial(url)
		if err != nil {
			log.Printf("Failed to dial\turl=%s err=%q\n", url, err)
		} else {
			nodes = append(nodes, &Node{
				Number:       i,
				Client:       client,
				AccountStore: as,
			})
		}
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("failed to dial all nodes\turls=%d", len(urls))
	}
	return nodes, nil
}

func run(nodes []*Node) error {
	ctx, cancelFn := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for range sigCh {
			cancelFn()
		}
	}()

	seedCh := make(chan SeedReq)
	var wg sync.WaitGroup
	var seeders int
	for _, node := range nodes {
		acct, err := node.NextSeed()
		if err != nil {
			log.Printf("Failed to create account seeder\terr=%q\n", err)
			continue
		}
		s := &Seeder{
			Node: node,
			acct: acct,
		}
		wg.Add(1)
		seeders++
		go s.Run(ctx, seedCh, wg.Done)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if seeders == 0 {
		return fmt.Errorf("failed to create any seeders\tcount=%d", len(nodes))
	}
	log.Printf("Started seeders\tcount=%d\n", seeders)

	sendBlock, err := waitForBlock(ctx, nodes[0].Client, 0)
	if err != nil {
		// Cancelled.
		return err
	}
	log.Printf("Starting senders\tcount=%d block=%d\n", config.senders, sendBlock)
	stats := NewReporter()
	if config.dur != 0 {
		t := time.AfterFunc(config.dur, func() {
			cancelFn()
		})
		defer t.Stop()
	}

	wg.Add(config.senders)
	txs := make(chan struct{}, config.tps)

	for n := 0; n < config.senders; n++ {
		s := Sender{
			Number: n,
			Node:   nodes[n%len(nodes)],
			SeedCh: seedCh,
		}
		go s.Send(ctx, txs, wg.Done)
	}

	// 1/10 second batches, with reports every 30s.
	batch := time.NewTicker(time.Second / 10)
	report := time.NewTicker(30 * time.Second)
	defer batch.Stop()
	defer report.Stop()

	batchSize := config.tps / 10
	var reports Reports

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-report.C:
			reports.Add(stats.Report())
			l, r, t := reports.Status()
			log.Printf("Report - total\t%s\n", &t)
			log.Printf("Report - recent\t%s\n", &r)
			log.Printf("Report - latest\t%s\n", &l)
		case <-batch.C:
			for i := 0; i < batchSize; i++ {
				txs <- struct{}{}
			}
		}
	}
	close(txs)
	cancelFn()
	wg.Wait()

	reports.Add(stats.Report())
	l, r, t := reports.Status()
	log.Printf("Final report - total\t%s\n", &t)
	log.Printf("Final report - recent\t%s\n", &r)
	log.Printf("Final report - latest\t%s\n", &l)
	bigBlock, err := nodes[0].LatestBlockNumber(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		log.Printf("Failed to get block number\terr=%q\n", err)
	} else {
		log.Printf("Ran between blocks\tstart=%d end=%d\n", sendBlock, bigBlock)
	}
	return nil
}

func waitForBlock(ctx context.Context, client *ethclient.Client, blockNum uint64) (uint64, error) {
	for {
		t := time.Now()
		big, err := client.LatestBlockNumber(ctx)
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		latestBlockNumberTimer.UpdateSince(t)
		if err != nil {
			log.Printf("Failed to get block number\terr=%q\n", err)
		} else {
			current := big.Uint64()
			if current >= blockNum {
				return current, nil
			}
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}