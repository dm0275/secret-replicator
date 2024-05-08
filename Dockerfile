FROM alpine

COPY ./build/secret-replicator-linux-amd64 secret-replicator

CMD ./secret-replicator