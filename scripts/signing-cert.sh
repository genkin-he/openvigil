#!/bin/sh
# 在登录钥匙串里建一张只用来签 vigil 的自签证书（只需跑一次）。
# 之后 make install 会用它签名，macOS 授予 vigil 的权限在升级后依然有效。
set -eu
NAME="${SIGN_ID:-Vigil Local Signing}"
if security find-certificate -c "$NAME" >/dev/null 2>&1; then
	echo "证书「${NAME}」已经在钥匙串里了"
	exit 0
fi
DIR=$(mktemp -d)
trap 'rm -rf "$DIR"' EXIT
cat > "$DIR/cert.cnf" <<CNF
[req]
distinguished_name=dn
x509_extensions=ext
prompt=no
[dn]
CN=$NAME
[ext]
basicConstraints=critical,CA:false
keyUsage=critical,digitalSignature
extendedKeyUsage=critical,codeSigning
CNF
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$DIR/key.pem" -out "$DIR/cert.pem" -days 3650 -config "$DIR/cert.cnf" 2>/dev/null
PASS=$(openssl rand -hex 12)
openssl pkcs12 -export -legacy -inkey "$DIR/key.pem" -in "$DIR/cert.pem" -out "$DIR/vigil.p12" -passout "pass:$PASS" -name "$NAME" 2>/dev/null ||
	openssl pkcs12 -export -inkey "$DIR/key.pem" -in "$DIR/cert.pem" -out "$DIR/vigil.p12" -passout "pass:$PASS" -name "$NAME"
security import "$DIR/vigil.p12" -k "$HOME/Library/Keychains/login.keychain-db" -P "$PASS" -T /usr/bin/codesign
echo "已建好「${NAME}」。现在跑 make install，再在「App 管理」里最后开一次 vigil 的开关。"
