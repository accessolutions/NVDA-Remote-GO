FROM golang:1.25-alpine as build

RUN apk add --no-cache git gcc musl-dev
RUN mkdir /app
WORKDIR /app
COPY . .
RUN ln -s /usr/bin/gcc /usr/bin/musl-gcc && sh ./build-static.sh
FROM scratch

COPY --from=build /app/nvdaRemoteServer /nvdaRemoteServer
COPY --from=build /app/cert.pem /cert.pem

EXPOSE 6837 443
CMD ["/nvdaRemoteServer", "-conf-read=false", "-cert-file", "/cert.pem", "-key-file", "/cert.pem"]
