package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"
)

type Request struct {
	URL string `json:"url"`
}

type Response struct {
	Key string `json:"key"`
}

const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
const keyLength = 7

func toBase62(n uint64) string {
	if n == 0 {
		return string(alphabet[0])
	}

	buf := make([]byte, 0, 11)
	for n > 0 {
		buf = append(buf, alphabet[n%62])
		n /= 62
	}

	// Reverse the buffer to get the correct order
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}

	return string(buf)
}

// maxKey is 62^keyLength, the exclusive upper bound for a generated key.
// Computed once per cold start rather than on every invocation.
var maxKey = new(big.Int).Exp(big.NewInt(62), big.NewInt(keyLength), nil)

func padKey(key string) string {
	if len(key) < keyLength {
		return strings.Repeat(string(alphabet[0]), keyLength-len(key)) + key
	}
	return key
}

func generateKey() (string, error) {
	n, err := rand.Int(rand.Reader, maxKey)
	if err != nil {
		return "", fmt.Errorf("reading random source: %w", err)
	}

	key := toBase62(n.Uint64())

	// Left-pad so every key is exactly keyLength characters
	if len(key) < keyLength {
		key = padKey(key)
	}

	return key, nil
}

func handleRequest(ctx context.Context, request Request) (Response, error) {

	key, err := generateKey()
	if err != nil {
		return Response{}, fmt.Errorf("generating key: %w", err)
	}
	return Response{Key: key}, nil
}

func main() {
	lambda.Start(handleRequest)
}
