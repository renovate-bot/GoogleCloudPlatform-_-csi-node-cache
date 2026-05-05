.PHONY: all verify build-and-push setup-kustomize images
.PHONY: unit-test

TAG=v1.1.0
BUILD_ARGS=

DRIVER_IMAGE_NAME=csi-node-cache-driver
CONTROLLER_IMAGE_NAME=csi-node-cache-controller

# If REPO is defined, an artifact registry setup is used. Otherwise the legacy
# gcr.io is used.
ifdef REPO
PROJECT_REPO=$(PROJECT)/$(REPO)
REPO_HOST?=us-docker.pkg.dev
else
PROJECT_REPO=$(PROJECT)
REPO_HOST?=gcr.io
endif

all:
	@echo Select a target, eg verify

verify:
	hack/verify-all.sh

unit-test:
	go test -v -mod=vendor -timeout 30s "./pkg/..." -cover

build-and-push:
	@if [ -z "$(PROJECT)" ] ; then echo Missing PROJECT; false; fi
	@if [ -z "$(IMAGE)" ] ; then echo Missing IMAGE; false; fi
	@if [ -z "$(DOCKERFILE)" ] ; then echo Missing DOCKERFILE; false; fi
	docker build -t "$(REPO_HOST)/$(PROJECT_REPO)/$(IMAGE):$(TAG)" . --file "$(DOCKERFILE)" $(BUILD_ARGS) --progress=plain
	docker push "$(REPO_HOST)/$(PROJECT_REPO)/$(IMAGE):$(TAG)"

setup-kustomize:
	@if [ -z $(PROJECT) ] ; then echo Missing PROJECT; false; fi
	@if [ -z $(TAG) ] ; then echo Missing TAG. The same tag will be used for all images; false; fi
	@if [ -z $(DRIVER_IMAGE_NAME) ] ; then echo Missing DRIVER_IMAGE_NAME; false; fi
	@if [ -z $(CONTROLLER_IMAGE_NAME) ] ; then echo Missing CONTROLLER_IMAGE_NAME; false; fi
	sed "s|DRIVER_IMAGE_NAME|$(REPO_HOST)/$(PROJECT_REPO)/$(DRIVER_IMAGE_NAME)|" deploy/images.yaml.template \
	 | sed "s|CONTROLLER_IMAGE_NAME|$(REPO_HOST)/$(PROJECT_REPO)/$(CONTROLLER_IMAGE_NAME)|" \
   | sed "s|IMAGE_TAG|$(TAG)|" \
   > deploy/images.yaml

images: setup-kustomize
	$(MAKE) IMAGE=$(DRIVER_IMAGE_NAME) BUILD_ARGS="--build-arg VERSION=$(TAG)" DOCKERFILE=cmd/driver/Dockerfile build-and-push
	$(MAKE) IMAGE=$(CONTROLLER_IMAGE_NAME) DOCKERFILE=cmd/controller/Dockerfile build-and-push
