all: build
.PHONY: all

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

clean:
	$(RM) vmware-vsphere-csi-driver-operator
.PHONY: clean

GO_TEST_PACKAGES :=./pkg/... ./cmd/...

# Run e2e tests. Requires openshift-tests in $PATH.
#
# Example:
#   make test-e2e
test-e2e:
	hack/e2e.sh

.PHONY: test-e2e

operator-e2e-test: GO_TEST_PACKAGES :=./test/e2e/...
operator-e2e-test: GO_TEST_FLAGS += -v
operator-e2e-test: GO_TEST_FLAGS += -count=1
operator-e2e-test: GO_TEST_FLAGS += -timeout 3h
operator-e2e-test: GO_TEST_FLAGS += -p 1
operator-e2e-test: test-unit
.PHONY: operator-e2e-test
