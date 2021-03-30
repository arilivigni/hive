#!/bin/bash

# AppSRE team CD
# This is a buildah version of the docker script

set -exv

CURRENT_DIR=$(dirname $0)

# build image
buildah bud --tls-verify=$SSL_VERIFY \
--layers \
-f $DOCKERFILE_CONTEXT_DIR \
-t $QUAY_IMG:$GIT_HASH .

# deploy image with to quay.io with git hash to image tag
buildah push --tls-verify=$SSL_VERIFY \
$QUAY_IMG:$GIT_HASH \
$QUAY_IMG:$GIT_HASH

# deploy image to quay.io with latest tag
buildah push --tls-verify=$SSL_VERIFY \
$QUAY_IMG:$GIT_HASH \
$QUAY_IMG:latest
