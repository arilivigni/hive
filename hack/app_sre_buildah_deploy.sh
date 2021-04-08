#!/bin/bash

# AppSRE team CD
# This is a buildah version of the docker script

set -exv

CURRENT_DIR=$(dirname $0)

# build image
buildah bud --storage-driver=vfs \
--tls-verify=$SSL_VERIFY \
--layers \
-f $DOCKERFILE_CONTEXT_DIR/Dockerfile \
-t $QUAY_IMG:$GIT_HASH $DOCKERFILE_CONTEXT_DIR

# deploy image with to quay.io with git hash to image tag
buildah push --storage-driver=vfs \
--tls-verify=$SSL_VERIFY \
$QUAY_IMG:$GIT_HASH \
$QUAY_IMG:$GIT_HASH

# deploy image to quay.io with latest tag
buildah push --storage-driver=vfs \
--tls-verify=$SSL_VERIFY \
$QUAY_IMG:$GIT_HASH \
$QUAY_IMG:latest
