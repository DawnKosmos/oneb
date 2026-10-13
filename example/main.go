package main

import (
	"context"
	"flag"
	"log"
	"net/http"

	"github.com/DawnKosmos/oneb"
	"github.com/DawnKosmos/oneb/oneb_gen"
)

type EchoInput struct {
	Message string `json:"message"`
}

type EchoOutput struct {
	Message string `json:"message"`
}

type ArithmeticInput struct {
	Left  float64 `json:"left"`
	Right float64 `json:"right"`
}

type AddOutput struct {
	Sum float64 `json:"sum"`
}

type SubtractOutput struct {
	Difference float64 `json:"difference"`
}

func echo(_ context.Context, input EchoInput) (EchoOutput, error) {
	return EchoOutput{Message: input.Message}, nil
}

func add(_ context.Context, input ArithmeticInput) (AddOutput, error) {
	return AddOutput{Sum: input.Left + input.Right}, nil
}

func subtract(_ context.Context, input ArithmeticInput) (SubtractOutput, error) {
	return SubtractOutput{Difference: input.Left - input.Right}, nil
}

func main() {
	outputDir := flag.String("generate", "", "generate TypeScript models and client into this folder")
	flag.Parse()
	if *outputDir != "" {
		if err := oneb_gen.Generate(routes, *outputDir); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func routes(router *oneb.Router) {
	router.Add("echo", echo, oneb.WithStrictParams())

	math := router.Group("math")
	math.Add[ArithmeticInput, AddOutput]("add", add, oneb.WithStrictParams(), oneb.WithPositionalParams())
	math.Add("subtract", subtract, oneb.WithStrictParams(), oneb.WithPositionalParams())
}

func run() error {
	router := oneb.New()
	defer router.Close()
	routes(router)

	mux := http.NewServeMux()
	mux.Handle("/rpc", router)

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	log.Printf("JSON-RPC server listening on http://localhost:8080/rpc")
	return server.ListenAndServe()
}
