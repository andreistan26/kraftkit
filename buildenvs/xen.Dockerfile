# SPDX-License-Identifier: BSD-3-Clause
# Copyright (c) 2022, Unikraft GmbH and The KraftKit Authors.
# Licensed under the BSD-3-Clause License (the "License").
# You may not use this file except in compliance with the License.

FROM kraftkit.sh/base-golang:latest

RUN set -xe; \
    apt-get update; \
    apt-get install -y \
        cmake \
        libssh2-1-dev \
        binutils \
        bison \
        build-essential \
        flex \
        gcc \
        git \
        libncurses5-dev \
        libssl-dev \
        make \
        python3 \
        python3-pip \
        python3-setuptools \
        python3-wheel \
        ninja-build \
        perl \
        pkg-config \
        libglib2.0-dev \
        libpixman-1-dev \
        iasl \
        libyajl-dev \
        uuid-dev \
        libslirp-dev; \
    pip3 install python-config --break-system-packages; \
    git clone -b stable-4.18 https://xenbits.xen.org/git-http/xen.git /xen; \
    cd /xen; \
    ./configure; \
    make -j install-tools; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*;

WORKDIR /go/src/kraftkit.sh

COPY . .

ENV DOCKER=                                       
ENV GOROOT=/usr/local/go                          
ENV KRAFTKIT_LOG_LEVEL=debug                      
ENV KRAFTKIT_LOG_TYPE=basic                       
ENV PAGER=cat                                     
ENV PATH=$PATH:/go/src/kraftkit.sh/dist           
