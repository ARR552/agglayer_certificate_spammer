# Agglayer certificate spammer

```
clear && rm databases/aggsender* && go1.23.5 run ./cmd valid-certs --cfg ./config.toml
```
Command examples:
```
clear && go1.23.5 run ./cmd valid-certs --cfg ./config.toml --empty-cert --add-fake-bridge --store-certificate --single-cert

clear && go1.23.5 run ./cmd invalid-signature-certs --cfg ./config.toml --empty-cert --add-fake-bridge --store-certificate --single-cert

clear && go1.23.5 run ./cmd random-certs --url https://agglayer-dev.polygon.technology --private-key 0x45f3ccdaff88ab1b3bb41472f09d5cde7cb20a6cbbc9197fddf64e2f3d67aaf2 --valid-signature --empty-cert --network-id 12 --random-global-index

clear && go1.23.5 run ./cmd random-certs --url $(kurtosis port print cdk agglayer agglayer) --private-key 0x45f3ccdaff88ab1b3bb41472f09d5cde7cb20a6cbbc9197fddf64e2f3d67aaf2 --valid-signature --empty-cert --network-id 12 --random-global-index


```
