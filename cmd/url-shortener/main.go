package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	neturl "net/url"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// maxKeyAttempts bounds the generate-and-retry loop. Keys are random over
// 62^7 (~3.5e12), so a collision is rare; several in a row means the table is
// near-full or the generator is broken — fail loudly rather than loop forever.
const maxKeyAttempts = 5

// maxURLLength caps what we are willing to store and later send back in a
// Location header.
const maxURLLength = 2048

// originSecretHeader is added by CloudFront to every request it forwards. API
// Gateway lowercases header names in the event.
const originSecretHeader = "x-origin-verify"

type ShortenRequest struct {
	URL string `json:"url"`
}

// Mirrors the key-generator's Response. Duplicated deliberately: the two
// Lambdas are separate modules and the JSON is the contract between them.
type KeyGeneratorResponse struct {
	Key string `json:"key"`
}

// What this Lambda needs from the key-generator Lambda. Narrow on purpose:
// a test fake only has to implement what we actually call.
type keyGenerator interface {
	Invoke(context.Context, *awslambda.InvokeInput, ...func(*awslambda.Options)) (*awslambda.InvokeOutput, error)
}

// What this Lambda needs from DynamoDB.
type urlPutter interface {
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
}

type handler struct {
	keygen          keyGenerator
	ddb             urlPutter
	tableName       string
	keyGeneratorARN string

	// Shared secret CloudFront sends in originSecretHeader. Empty disables the
	// check (local runs and tests); the template always sets it when enforcing.
	originSecret string
}

func badRequest(message string) events.APIGatewayV2HTTPResponse {
	return events.APIGatewayV2HTTPResponse{
		StatusCode: 400,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       fmt.Sprintf(`{"error":%q}`, message),
	}
}

func forbidden() events.APIGatewayV2HTTPResponse {
	return events.APIGatewayV2HTTPResponse{
		StatusCode: 403,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       `{"error":"forbidden"}`,
	}
}

// fromEdge reports whether the request carries the secret CloudFront adds. It
// is what keeps the raw execute-api URL from bypassing the WAF.
func (h *handler) fromEdge(request events.APIGatewayV2HTTPRequest) bool {
	if h.originSecret == "" {
		return true
	}
	got := request.Headers[originSecretHeader]
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.originSecret)) == 1
}

// validateURL accepts only absolute http(s) URLs with a host and no embedded
// credentials. Anything else is a way to turn the redirect into a javascript:
// or file: link, or a host-spoofing "https://trusted.com@evil.com" link.
func validateURL(raw string) error {
	if len(raw) > maxURLLength {
		return fmt.Errorf("url is too long (max %d characters)", maxURLLength)
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f {
			return errors.New("url must not contain spaces or control characters")
		}
	}
	u, err := neturl.Parse(raw)
	if err != nil {
		return errors.New("url is not valid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("url must start with http:// or https://")
	}
	if u.Hostname() == "" {
		return errors.New("url must include a host")
	}
	if u.User != nil {
		return errors.New("url must not contain a username or password")
	}
	return nil
}

func (h *handler) Shorten(
	ctx context.Context,
	request events.APIGatewayV2HTTPRequest,
) (events.APIGatewayV2HTTPResponse, error) {
	// Tags every line below, so one request is greppable end to end.
	log := slog.With("request_id", request.RequestContext.RequestID)

	if !h.fromEdge(request) {
		log.Warn("request did not come through the edge")
		return forbidden(), nil
	}

	body := request.Body
	if request.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			log.Warn("body is not valid base64", "error", err)
			return badRequest("request body is not valid base64"), nil
		}
		body = string(decoded)
	}

	var req ShortenRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		log.Warn("body is not valid JSON", "error", err)
		return badRequest("request body must be valid JSON"), nil
	}
	if req.URL == "" {
		log.Warn("url missing from body")
		return badRequest("url is required"), nil
	}
	if err := validateURL(req.URL); err != nil {
		log.Warn("url rejected", "reason", err.Error())
		return badRequest(err.Error()), nil
	}

	log.Info("shorten requested", "url", req.URL)

	keygenPayload, err := json.Marshal(req)
	if err != nil {
		return events.APIGatewayV2HTTPResponse{}, err
	}

	key, err := h.store(ctx, log, req.URL, keygenPayload)
	if err != nil {
		return events.APIGatewayV2HTTPResponse{}, err
	}

	respBody, err := json.Marshal(KeyGeneratorResponse{Key: key})
	if err != nil {
		return events.APIGatewayV2HTTPResponse{}, err
	}

	return events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(respBody),
	}, nil
}

// store gets a key from the generator and writes {key, url, created_at} with a
// write that fails rather than overwrites if the key is already taken. On that
// specific failure it regenerates and retries, bounded by maxKeyAttempts.
func (h *handler) store(ctx context.Context, log *slog.Logger, url string, keygenPayload []byte) (string, error) {
	for attempt := 1; attempt <= maxKeyAttempts; attempt++ {
		key, err := h.requestKey(ctx, log, keygenPayload)
		if err != nil {
			return "", err
		}

		err = h.putUnique(ctx, key, url)
		if err == nil {
			log.Info("key saved", "key", key, "url", url, "attempts", attempt)
			return key, nil
		}

		var collision *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &collision) {
			log.Warn("key already taken, regenerating", "key", key, "attempt", attempt)
			continue
		}

		log.Error("saving key", "error", err, "key", key, "table", h.tableName)
		return "", fmt.Errorf("saving key: %w", err)
	}

	log.Error("gave up finding a free key", "attempts", maxKeyAttempts)
	return "", fmt.Errorf("no free key after %d attempts", maxKeyAttempts)
}

// requestKey calls the key-generator Lambda synchronously and returns its key.
func (h *handler) requestKey(ctx context.Context, log *slog.Logger, payload []byte) (string, error) {
	out, err := h.keygen.Invoke(ctx, &awslambda.InvokeInput{
		FunctionName:   aws.String(h.keyGeneratorARN),
		InvocationType: types.InvocationTypeRequestResponse, // sync — we need the key back
		Payload:        payload,
	})
	if err != nil {
		log.Error("invoking key-generator", "error", err)
		return "", fmt.Errorf("invoking key-generator: %w", err)
	}

	if out.FunctionError != nil {
		// The generator's handler returned an error; the Invoke itself succeeded.
		log.Error("key-generator returned an error",
			"function_error", *out.FunctionError, "payload", string(out.Payload))
		return "", fmt.Errorf("key-generator failed (%s): %s", *out.FunctionError, out.Payload)
	}

	var keyResp KeyGeneratorResponse
	if err := json.Unmarshal(out.Payload, &keyResp); err != nil {
		log.Error("decoding key-generator response", "error", err, "payload", string(out.Payload))
		return "", fmt.Errorf("decoding key-generator response: %w", err)
	}
	if keyResp.Key == "" {
		log.Error("key-generator returned an empty key")
		return "", fmt.Errorf("key-generator returned an empty key")
	}

	log.Info("key generated", "key", keyResp.Key)
	return keyResp.Key, nil
}

// putUnique writes the row only if the key is not already present. A taken key
// comes back as *ddbtypes.ConditionalCheckFailedException.
func (h *handler) putUnique(ctx context.Context, key, url string) error {
	_, err := h.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(h.tableName),
		Item: map[string]ddbtypes.AttributeValue{
			"key":        &ddbtypes.AttributeValueMemberS{Value: key},
			"url":        &ddbtypes.AttributeValueMemberS{Value: url},
			"created_at": &ddbtypes.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
		},
		// "key" is a DynamoDB reserved word, so reference it via a name placeholder.
		ConditionExpression:      aws.String("attribute_not_exists(#k)"),
		ExpressionAttributeNames: map[string]string{"#k": "key"},
	})
	return err
}

func main() {
	// Built here rather than in init() so tests never touch the AWS SDK.
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(err)
	}

	// JSON so CloudWatch Insights can filter on the fields directly.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	h := &handler{
		keygen:          awslambda.NewFromConfig(cfg),
		ddb:             dynamodb.NewFromConfig(cfg),
		tableName:       os.Getenv("URL_TABLE_NAME"),
		keyGeneratorARN: os.Getenv("KEY_GENERATOR_FUNCTION_ARN"),
		originSecret:    os.Getenv("ORIGIN_SECRET"),
	}
	lambda.Start(h.Shorten)
}
