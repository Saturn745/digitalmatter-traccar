FROM nixos/nix@sha256:7894650fb65234b35c80010e6ca44149b70a4a216118a6b7e5c6f6ae377c8d21 as builder
# Copy your project source
COPY . /src
WORKDIR /src
RUN nix build .#digitalmatter-traccar --experimental-features 'nix-command flakes'

FROM alpine@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1 as user-builder
RUN adduser -D -u 1000 -h /home/container container

FROM 11notes/distroless@sha256:5f470d4462eecb716351eb7ef9f9cb35df27dccc4fa76236f292bc72c8f1a58c
COPY --from=user-builder /etc/passwd /etc/passwd
COPY --from=user-builder /etc/group /etc/group  
COPY --from=user-builder /home/container /home/container
COPY --from=builder /nix/store/*/bin/digitalmatter-traccar /usr/local/bin/digitalmatter-traccar

WORKDIR /home/container
USER container
ENTRYPOINT ["/usr/local/bin/digitalmatter-traccar"]
