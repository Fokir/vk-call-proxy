.PHONY: bundle bundle-verify keygen

# Пересобрать bundled fallback для internal/scripts/ из hot-scripts/
# Запускать после изменений в hot-scripts/, чтобы первая установка приложения
# работала без сети.
bundle:
	go run ./tools/scripts-sign bundle -src hot-scripts -dst internal/scripts/bundled

# Сгенерировать пару Ed25519 ключей (приватный хранить в secrets, публичный —
# зашивать в бинарь через ldflags -X .../internal/scripts.DefaultPublicKey=...).
keygen:
	go run ./tools/scripts-sign keygen -out ./secrets

# Проверить подпись manifest'а в hot-scripts/ публичным ключом.
# Использование: make bundle-verify PUB="<base64>"
bundle-verify:
	@if [ -z "$(PUB)" ]; then echo "Usage: make bundle-verify PUB=<base64>"; exit 1; fi
	go run ./tools/scripts-sign verify -manifest hot-scripts/manifest.json -pub "$(PUB)"
