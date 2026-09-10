.PHONY: build cache-up cache-down cache-status

REGION      ?= us-east-1
CACHE_STACK ?= url-shortener-cache

build:
	sam build

# Step 4 Layer 2: the ElastiCache stack is created per session and destroyed
# before the laptop closes — it is the first thing that bills at rest.
cache-up:
	aws cloudformation deploy \
	  --template-file cache/template.yaml \
	  --stack-name $(CACHE_STACK) --region $(REGION) \
	  --no-fail-on-empty-changeset
	aws ssm get-parameter --name /url-shortener/cache/endpoint \
	  --region $(REGION) --query Parameter.Value --output text

cache-down:
	aws cloudformation delete-stack --stack-name $(CACHE_STACK) --region $(REGION)
	aws cloudformation wait stack-delete-complete --stack-name $(CACHE_STACK) --region $(REGION)
	@echo "cache stack deleted"

cache-status:
	aws cloudformation describe-stacks --stack-name $(CACHE_STACK) --region $(REGION) \
	  --query "Stacks[0].StackStatus" --output text 2>/dev/null || echo "not deployed"
