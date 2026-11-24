#!/usr/bin/env python3
# 把 EMQX 内置账号表里的口令散列抄进 eco_iot.mqtt_credential。
# 只打印是否对上，不打印口令、散列和盐。
import hashlib
import os
import pathlib
import subprocess
import sys
import tempfile

BROKER = os.environ.get("EMQX_CONTAINER", "eco-device-emqx")
MYSQL = os.environ.get("ECO_MYSQL_CONTAINER", "eco-mysql")
ENV_FILE = os.environ.get("ECO_DEVICE_ENV", "/data/eco-device-server/.env")


def redact(text):
    import re
    return re.sub(r"[0-9a-fA-F]{20,}", "[hex]", text or "")


def run(args, data=None, check=True):
    result = subprocess.run(args, input=data, capture_output=True, text=True)
    if check and result.returncode != 0:
        sys.stderr.write("command failed\n")
        sys.stderr.write(redact(result.stderr)[:800] + "\n")
        sys.stderr.write(redact(result.stdout)[:400] + "\n")
        sys.exit(1)
    return result


def export_rows():
    erl = (
        'file:write_file("/tmp/mqtt_cred_export.tsv", '
        'iolist_to_binary([begin {_, User} = element(2, Rec), '
        '[User, <<"\\t">>, element(3, Rec), <<"\\t">>, element(4, Rec), <<"\\n">>] '
        "end || Rec <- ets:tab2list(emqx_authn_mnesia)]))."
    )
    result = run(["docker", "exec", BROKER, "emqx", "eval", erl])
    if result.stdout.strip() != "ok":
        sys.stderr.write("export failed\n")
        sys.exit(1)
    local = tempfile.NamedTemporaryFile(delete=False)
    local.close()
    os.chmod(local.name, 0o600)
    try:
        run(["docker", "cp", f"{BROKER}:/tmp/mqtt_cred_export.tsv", local.name])
    finally:
        run(["docker", "exec", BROKER, "rm", "-f", "/tmp/mqtt_cred_export.tsv"], check=False)
    rows = {}
    try:
        for line in pathlib.Path(local.name).read_text().splitlines():
            if not line:
                continue
            user, password_hash, salt = line.split("\t")
            rows[user] = (password_hash, salt)
    finally:
        os.remove(local.name)
    return rows


def load_env(path):
    out = {}
    for line in pathlib.Path(path).read_text().splitlines():
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        out[key] = value.strip().strip('"').strip("'")
    return out


def digest(password, salt, suffix):
    raw = (password + salt) if suffix else (salt + password)
    return hashlib.sha256(raw.encode()).hexdigest()


def sql_quote(text):
    if not text or any(ord(char) < 33 or ord(char) > 126 for char in text):
        sys.stderr.write("credential charset rejected\n")
        sys.exit(1)
    return "'" + text.replace("\\", "\\\\").replace("'", "''") + "'"


def mysql(sql):
    result = run(
        ["docker", "exec", "-i", MYSQL, "sh", "-c", 'mysql -N -uroot -p"$MYSQL_ROOT_PASSWORD" eco_iot'],
        data=sql,
    )
    return result.stdout


def check_passwords(rows):
    env = load_env(ENV_FILE)
    container = run(["docker", "exec", "eco-iot", "printenv", "ECO_MQTT_PASSWORD"]).stdout.strip()
    pairs = (
        ("root", "IOT_MQTT_DEVICE_PASSWORD", env.get("IOT_MQTT_DEVICE_PASSWORD", "")),
        ("eco-iot", "IOT_ECO_IOT_MQTT_PASSWORD", env.get("IOT_ECO_IOT_MQTT_PASSWORD", "")),
        ("eco-iot", "ECO_MQTT_PASSWORD", container),
    )
    ok = True
    for user, label, password in pairs:
        stored, salt = rows[user]
        matched = digest(password, salt, True) == stored
        print(f"{label} suffix-match {str(matched).lower()}")
        if not matched:
            ok = False
    return ok


def insert(rows):
    statements = []
    for user, (password_hash, salt) in sorted(rows.items()):
        quoted = (sql_quote(user), sql_quote(password_hash), sql_quote(salt))
        statements.append(
            "INSERT INTO mqtt_credential (username, password_hash, salt) VALUES ("
            + ", ".join(quoted)
            + ") ON DUPLICATE KEY UPDATE password_hash = "
            + quoted[1]
            + ", salt = "
            + quoted[2]
            + ";"
        )
    mysql("\n".join(statements) + "\n")
    readback = mysql("SELECT username, password_hash, salt FROM mqtt_credential ORDER BY username;\n")
    got = {}
    for line in readback.splitlines():
        user, password_hash, salt = line.split("\t")
        got[user] = (password_hash, salt)
    print("copied", sorted(rows), "readback-match", str(got == rows).lower())
    return got == rows


def main():
    rows = export_rows()
    expected = {"root", "eco-iot"}
    if set(rows) != expected:
        print("unexpected-users", sorted(rows))
        sys.exit(1)
    for user, (password_hash, salt) in rows.items():
        print(user, "hashlen", len(password_hash), "saltlen", len(salt))
    if not check_passwords(rows):
        sys.stderr.write("password hash does not match\n")
        sys.exit(1)
    if "--insert" in sys.argv and not insert(rows):
        sys.exit(1)


if __name__ == "__main__":
    main()
