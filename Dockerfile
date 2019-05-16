FROM registry.access.redhat.com/ubi8/ubi-minimal AS build

ENV GOPATH=/go

RUN microdnf update --nodocs -y \
 && microdnf install --nodocs -y findutils git go-toolset make perl-Digest-SHA \
 && microdnf clean all -y \
 && rm -rf /var/cache/yum

ARG DEP_VERSION=v0.5.3

RUN mkdir -p "${GOPATH}/src/github.com/golang" \
 && cd "${GOPATH}/src/github.com/golang" \
 && git clone --depth 1 --recurse-submodules --shallow-submodules \
      --branch "${DEP_VERSION}" https://github.com/golang/dep.git dep \
 && cd dep \
 && mkdir -p "${GOPATH}/bin" \
 && make install

ARG BUILDDIR="${GOPATH}/src/github.com/3scale/3scale-istio-adapter"
WORKDIR "${BUILDDIR}"

ARG VERSION=

ADD . "${BUILDDIR}"
RUN PATH="${PATH}:${GOPATH//://bin:}/bin" \
 && if test "x${VERSION}" = "x"; then \
      VERSION="$(git describe --dirty --tags || true)" ; \
    fi \
 && make VERSION="${VERSION:? *** No VERSION could be derived, please specify it}" \
      build-adapter build-cli

FROM registry.access.redhat.com/ubi8/ubi-minimal

WORKDIR /app
COPY --from=build "${BUILDDIR}"/_output/3scale-istio-adapter /app/
COPY --from=build "${BUILDDIR}"/_output/3scale-config-gen /app/
ENV THREESCALE_LISTEN_ADDR 3333
EXPOSE 3333
EXPOSE 8080

CMD ./3scale-istio-adapter

