package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// What this Lambda needs from DynamoDB. Small on purpose: a test fake
// only has to implement what we actually call.
type dynamoGetter interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

type handler struct {
	ddb   dynamoGetter
	table string
}

func errorResponse(status int, message string) events.APIGatewayV2HTTPResponse {
	return events.APIGatewayV2HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body:       message,
	}
}

func (h *handler) Redirect(
	ctx context.Context,
	request events.APIGatewayV2HTTPRequest,
) (events.APIGatewayV2HTTPResponse, error) {
	// Tags every line below, so one request is greppable end to end.
	log := slog.With("request_id", request.RequestContext.RequestID)

	// Comes from the {key} in the route path.
	key := request.PathParameters["key"]
	if key == "" {
		log.Warn("no key in path")
		return errorResponse(400, "key is required"), nil
	}

	out, err := h.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(h.table),
		Key: map[string]ddbtypes.AttributeValue{
			"key": &ddbtypes.AttributeValueMemberS{Value: key},
		},
	})
	if err != nil {
		log.Error("looking up key", "error", err, "key", key, "table", h.table)
		return errorResponse(500, "internal error"), nil
	}

	// No item means the key was never issued, or was deleted.
	if len(out.Item) == 0 {
		log.Info("key not found", "key", key)
		return errorResponse(404, "not found"), nil
	}

	// Stored as a string attribute by url-shortener.
	urlAttr, ok := out.Item["url"].(*ddbtypes.AttributeValueMemberS)
	if !ok || urlAttr.Value == "" {
		log.Error("row has no usable url attribute", "key", key)
		return errorResponse(500, "internal error"), nil
	}

	log.Info("redirecting", "key", key, "url", urlAttr.Value)

	return events.APIGatewayV2HTTPResponse{
		StatusCode: 302,
		Headers: map[string]string{
			"Location": urlAttr.Value,
			// Keep the redirect out of caches while the mapping can still change.
			"Cache-Control": "no-store",
		},
	}, nil
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
		ddb:   dynamodb.NewFromConfig(cfg),
		table: os.Getenv("URL_TABLE_NAME"),
	}
	lambda.Start(h.Redirect)
}
