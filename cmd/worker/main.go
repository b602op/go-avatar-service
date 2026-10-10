package main

import (
	"fmt"
	"log"
	"time"
)

func main() {
	log.Println("Starting worker...")
	for {
		fmt.Println("Worker is running...")
		time.Sleep(10 * time.Second)
	}
}
