all: build build-tests-ext
.PHONY: all

# Override GO_PACKAGE detection to use only the root module path
# (workspace mode returns multiple modules which breaks build-machinery-go)
GO_PACKAGE := github.com/openshift/vmware-vsphere-csi-driver-operator

# Include the library makefile
include $(addprefix ./vendor/github.com/openshift/build-machinery-go/make/, \
	golang.mk \
	targets/openshift/deps-gomod.mk \
	targets/openshift/images.mk \
)

# Run core verification and all self contained tests.
#
# Example:
#   make check
check: | verify test-unit
.PHONY: check

IMAGE_REGISTRY?=registry.ci.openshift.org

# This will call a macro called "build-image" which will generate image specific targets based on the parameters:
# $0 - macro name
# $1 - target name
# $2 - image ref
# $3 - Dockerfile path
# $4 - context directory for image build
# It will generate target "image-$(1)" for building the image and binding it as a prerequisite to target "images".
$(call build-image,vmware-vsphere-csi-driver-operator,$(IMAGE_REGISTRY)/ocp/4.8:vmware-vsphere-csi-driver-operator,./Dockerfile.openshift,.)

# Build the test extension binary from the openshift-tests workspace
build-tests-ext:
	cd openshift-tests && $(GO) build $(GO_BUILD_FLAGS) -o ../vmware-vsphere-csi-driver-operator-tests-ext ./cmd/vmware-vsphere-csi-driver-operator-tests-ext
.PHONY: build-tests-ext

# Vendor dependencies using workspace-level vendoring
# This creates a single shared vendor directory for both modules
vendor:
	$(GO) mod tidy
	cd openshift-tests && $(GO) mod tidy
	$(GO) work vendor
.PHONY: vendor

clean:
	$(RM) vmware-vsphere-csi-driver-operator vmware-vsphere-csi-driver-operator-tests-ext
.PHONY: clean

GO_TEST_PACKAGES :=./pkg/... ./cmd/...

# Run e2e tests. Requires openshift-tests in $PATH.
#
# Example:
#   make test-e2e
test-e2e:
	hack/e2e.sh

.PHONY: test-e2e
