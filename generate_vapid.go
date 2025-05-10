package main

import (
	"github.com/SherClockHolmes/webpush-go"
	"log"
	"os"
)

func main() {
	pub, priv, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		log.Fatal(err)
	}

	f, err := os.OpenFile("vapid_public_key", os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	f.Write([]byte(pub))

	f, err = os.OpenFile("vapid_private_key", os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	f.Write([]byte(priv))
}
