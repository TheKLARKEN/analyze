SHELL := /bin/sh

MAKEFILE_PATH := $(abspath $(lastword $(MAKEFILE_LIST)))
CURRENT_DIR := $(patsubst %/,%,$(dir $(MAKEFILE_PATH)))

DOCKER_IMAGE_NAME := supergiant/analyze
DOCKER_IMAGE_TAG := $(shell git describe --tags --always | tr -d v || echo 'latest')


define LINT
	@echo "Running code linters..."
	revive
	@echo "Running code linters finished."
endef

define GOIMPORTS
	goimports -v -w -local github.com/supergiant/robot ${CURRENT_DIR}
endef

define TOOLS
		if [ ! -x "`which revive 2>/dev/null`" ]; \
        then \
        	@echo "revive linter not found."; \
        	@echo "Installing linter... into ${GOPATH}/bin"; \
        	go get -u github.com/mgechev/revive ; \
        fi

        if [ ! -x "`which swagger 2>/dev/null`" ]; \
        then \
        	@echo "swagger not found."; \
        	@echo "Installing swagger... into ${GOPATH}/bin"; \
        	go get -u github.com/go-swagger/go-swagger/cmd/swagger ; \
        fi
endef


.PHONY: default
default: lint


.PHONY: lint
lint: tools
	@$(call LINT)

.PHONY: validate
validate: tools
	swagger validate ./swagger/api-spec.yml

.PHONY: gen
gen: validate
	swagger generate model \
    		--target=./pkg \
    		--spec=./swagger/api-spec.yml
	swagger generate server \
		--target=./pkg \
		--server-package=api \
		--spec=./swagger/api-spec.yml \
		--exclude-main \
		--name=analyze
		--existing-models=./pkg/models
	cp ./swagger/api-spec.yml ./swagger/ui/api-spec.yml
	statik -f -src=${CURRENT_DIR}/swagger/ui

.PHONY: test
test:
	go test -race ./...

.PHONY: tools
tools:
	@$(call TOOLS)

.PHONY: goimports
goimports:
	@$(call GOIMPORTS)

.PHONY: build-image
build-image:
	docker build -t $(DOCKER_IMAGE_NAME):$(DOCKER_IMAGE_TAG) .
	docker tag $(DOCKER_IMAGE_NAME):$(DOCKER_IMAGE_TAG) $(DOCKER_IMAGE_NAME):latest

.PHONY: push
push:
	docker push $(DOCKER_IMAGE_NAME):latest

.PHONY: gofmt
gofmt:
	go fmt ./...

.PHONY: fmt
fmt: gofmt goimports


#all dependencies are "go get"-able for general dev environment usability.
# To compile all protobuf files in this repository, run "make protobuf"
.PHONY: protobuf
protobuf:
	docker run --rm -v ${CURRENT_DIR}/pkg/plugin:/defs namely/protoc-all:1.16_0 -i proto -l go -d /defs -o .