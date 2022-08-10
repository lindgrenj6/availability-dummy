FROM docker.io/golang:1.18 as build

RUN mkdir /build
WORKDIR /build

COPY go.mod .
RUN go mod download

COPY . .
RUN go build

FROM registry.access.redhat.com/ubi8/ubi-minimal:8.6
COPY --from=build /build/availability-dummy /availability-dummy
ENTRYPOINT ["/availability-dummy"]
