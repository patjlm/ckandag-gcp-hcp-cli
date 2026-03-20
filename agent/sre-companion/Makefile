IMAGE ?= quay.io/patmarti/sre-companion-agent:latest
PROJECT_ID ?= $(shell gcloud config get-value project 2>/dev/null)
REGION ?= us-central1
AR_IMAGE ?= $(REGION)-docker.pkg.dev/$(PROJECT_ID)/quay-io/patmarti/sre-companion-agent:latest

.PHONY: build push deploy refresh terraform-apply all

build:
	podman build -t $(IMAGE) .

push:
	podman push $(IMAGE)

refresh:
	gcloud run services update sre-companion-agent \
		--region $(REGION) \
		--project $(PROJECT_ID) \
		--image $(AR_IMAGE)

deploy: build push refresh

terraform-apply:
	cd deploy && terraform init -upgrade && \
		terraform apply -var="project_id=$(PROJECT_ID)" -var="region=$(REGION)"

all: build push terraform-apply refresh
