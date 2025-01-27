# Agglayer certificate spammer

```
clear && rm databases/aggsender* && go1.23.5 run ./cmd valid-certs --cfg ./config.toml
```
Command examples:
```
clear && go1.23.5 run ./cmd valid-certs --cfg ./config.toml --empty-cert --add-fake-bridge --store-certificate

clear && go1.23.5 run ./cmd invalid-signature-certs --cfg ./config.toml --empty-cert --add-fake-bridge --store-certificate

clear && go1.23.5 run ./cmd random-certs --url https://agglayer-dev.polygon.technology --private-key 0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80 --valid-signature --empty-cert

```
