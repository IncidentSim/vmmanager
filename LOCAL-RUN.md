# vmmanager: локальный запуск с настоящим Firecracker

← [Назад к README](README.md)

Репозиторий: `https://github.com/IncidentSim/vmmanager.git` (прежнее название — vibe-firecracker).
В проверенных командах ниже сохранён старый URL клонирования: GitHub перенаправляет его в тот же репозиторий.
Закреплённый коммит (main): `92797338416f5bd84341d8d08e2724cb76b1ae47`

**Статус проверки.** Команды руководства прошли на чистой Ubuntu 24.04 x86_64 (вложенный KVM, 1 ГБ RAM,
обычный пользователь `ubuntu` с passwordless sudo) на закреплённом коммите: установка → реальный Create →
SSH с pinning ключа хоста → Stop → cold Commit → Delete. Предполагается именно обычный пользователь `ubuntu`.
Проверен фоновый запуск (вариант B) и описанный жизненный цикл; вариант A (на переднем плане) как альтернатива отдельно не
тестировался. Не проверены: производный VM из committed-образа, TTL-истечение, перезапуск vmapi (см. §15).

Цель: на чистой выделенной Ubuntu 24.04 x86_64 VM (systemd, вложенный `/dev/kvm`, 1 ГБ RAM,
интернет, пользователь `ubuntu` с passwordless sudo) собрать и запустить vmmanager и пройти
цепочку: **Create → poll → SSH-метаданные → аутентифицированный SSH → Stop → cold Commit → Delete (VM и образа)**.

## 0. Как читать и исполнять

* Руководство написано по исходникам коммита выше, а не по старой документации (где она расходится
  с кодом — см. §14). Границы проверки — в начале документа и в §15.
  Всё, что не проверено, помечено «не проверено».
* **Останавливайтесь на первой же неописанной ошибке** и сообщайте вывод; не импровизируйте.
* Каждый блок кода — bash-скрипт. Запускайте блок как `bash -euo pipefail` (например
  `bash -euo pipefail <<'EOF' … EOF`), а не построчно в интерактивной оболочке.
  Метки: `# [user]` — от `ubuntu`, `# [sudo]` — отдельные команды с `sudo`. Нигде не работайте под `su -`/root-оболочкой:
  владение файлами важно (vmapi работает как `ubuntu`, не root).
* Блоки, использующие переменные, начинаются с `source ~/vmlab/lab.env` (создаётся в §2).
* Секреты (токен, ключи, мастер-ключ) **не печатайте** и не включайте в отчёт. Печатайте только факт наличия/права.
* Две оболочки: **оболочка A** — сервер `vmapi` (§10), **оболочка B** — curl/ssh (§11–§13). Если у вас одна оболочка —
  запускайте vmapi в фоне, как показано в §10 (nohup).

## 1. Что именно будет построено и почему (контракт из исходников)

* `vmapi` (непривилегированный, пользователь `ubuntu`) слушает `127.0.0.1:8090` (plain HTTP допустим только на loopback,
  `internal/controlapi/config.go: Validate`). Аутентификация — `Authorization: Bearer <токен>` из файла 0600
  (`Credential.Resolve`). Telegram/JWT/Nexterm/ttyd/TLS не используются: `terminal` в конфиге **опущен**
  (в примерах `deployment/vmapi/vmapi.example.json` он есть и открывает `0.0.0.0:8443` — **не копировать**).
* Привилегированная часть — только systemd-юниты, установленные `vmhost install`: `fc-vm-helper.socket`
  (`/run/fc-vm-helper.sock`, root:`ubuntu` 0660; единственная команда `start-slot N CPU_MILLI`),
  `firecracker-vm@N.service` (jail + Firecracker), `firecracker-vm-network@N.service` (TAP/NAT/firewall).
* **Внутренние TAP/NAT/forwarding — требование реализации**, а не публикация наружу. Backend всегда
  выдаёт гостю адрес слота `<prefix>.N.2/30`, шлюз `.1` на `tap-vmN`; без TAP гость недостижим даже с хоста.
  `firecracker-vm@N` имеет `Requires=firecracker-vm-network@N`, а `vmhost preflight` падает при `ip_forward!=1`.
  Публичных IP/портов (`PUBLIC_IPV4`/`SSH_PORT_BASE`) мы **не** задаём; SSH идёт с хоста напрямую на `10.222.0.2:22`.
  Сетевой инструмент создаёт правила только своих цепочек `FCVM0*` и ничего не публикует.
* Каталог образов: `<images_dir>/<ID>.ext4` — обычный файл (не symlink); базовый образ — тот, у которого нет маркера
  `.committed/<ID>` (`vmlib/local/backend.go: imageInfo`). ID базового образа обязан быть в `shared_image_ids` API.
* Хранилище: backend требует, чтобы `storage_mount` был точкой монтирования (`st_dev` отличается от родителя) и
  `work_root`/`images_dir` лежали на том же устройстве (`checkStorage`); юнит повторяет это через
  `AssertPathIsMountPoint`. Поэтому: **отдельный разреженный файл → ext4 → loop-mount** в
  `/var/lib/firecracker-builder/storage` (путь по умолчанию в `vmhost`). Физические диски не форматируются.
* Create DTO (`internal/controlapi/validate.go`): только `name`, `image_id`, `profile`, `ttl_seconds`, `ssh_public_key`.
  Поле `resources` отвергается. TTL по умолчанию 3600, максимум 86400. `name` — метка
  `^[a-z]([a-z0-9-]{0,18}[a-z0-9])?$`, без префикса `bot-`. ID профиля/образа — `^[a-z0-9][a-z0-9-]{0,62}$` (**без `_`**).
* Образ гостя обязан подойти офлайн-провижинерам: ext4 без раздела, `ID=ubuntu`, пользователь `root` с `/root`,
  **точная** копия pinned Firecracker-CI `fcnet.service` + `fcnet-setup.sh`, включённый в `sysinit.target.wants`
  (`internal/sshaccess/network.go`, `provision.go`, `supported/*`). Это сверяется в §6.
* Backend не растит диск: `disk_mib` лишь верхняя граница для размера файла образа (`образ > disk_mib` → отказ).
* Слот: 1 (`MAX_SLOTS=1`, `max_vms=1`), профиль 1 vCPU / 256 MiB. Лимит юнита `MemoryMax=768M` ⇒
  `unit_mem_limit_mib=768`, `256+64 ≤ 768` выполнено. Профиль 256 MiB проверен реальной загрузкой и SSH в этом nested-KVM окружении; для иных образов его может быть мало.

## 2. Предусловия и переменные окружения

```bash
# [user] проверки, ничего не меняют
set -euo pipefail
. /etc/os-release; echo "$ID $VERSION_ID"          # ожидается: ubuntu 24.04
uname -m                                           # ожидается: x86_64
id -un                                             # ожидается: ubuntu
sudo -n true && echo sudo-ok
test -c /dev/kvm && echo kvm-ok                    # вложенный KVM
test -d /run/systemd/system && echo systemd-ok
stat -fc %T /sys/fs/cgroup                         # ожидается: cgroup2fs (cgroup v2, нужен для CPUQuota)
free -m; df -h /                                   # свободно на / нужно >= 12 GiB (размеры артефактов точно не известны)
ip -4 route show default                           # ровно ОДНА строка (требование fc-vm-network)
```

Если какой-то пункт не выполнен — остановитесь.

Создайте рабочий каталог и файл переменных (`~/vmlab` с правами 0700 важен: backend проверяет «цепочку предков»
мастер-ключа и known_hosts — они не должны быть доступны на запись группе/всем):

```bash
# [user]
set -euo pipefail
install -d -m 0700 ~/vmlab
cat > ~/vmlab/lab.env <<'EOF'
export LAB="$HOME/vmlab"
export SRC="$LAB/src" DL="$LAB/dl" ART="$LAB/artifacts" IMGW="$LAB/imagework"
export ETC="$LAB/etc" SECRETS="$LAB/secrets" STATE="$LAB/state" RUNL="$LAB/run" LOG="$LAB/log"
export STORAGE=/var/lib/firecracker-builder/storage     # умолчание vmhost / юнитов
export STORAGE_IMG=/var/lib/vmlab/storage.img
export GPFX=10.222                                      # префикс гостевой сети (после проверки коллизий, §4)
export IMAGE_ID=lab-base PROFILE=lab256 MEM_MIB=256
export USER_ID=1                                        # синтетический положительный ID владельца
export BASE=http://127.0.0.1:8090
export GOROOT="$LAB/tools/go" GOPATH="$LAB/gopath" GOCACHE="$LAB/gocache" GOTOOLCHAIN=local
export GOMAXPROCS=1 GOFLAGS=-p=1 GOGC=50 GOMEMLIMIT=512MiB   # ограничение памяти сборки (значения не измерялись)
export PATH="$GOROOT/bin:$PATH"
EOF
chmod 0600 ~/vmlab/lab.env
source ~/vmlab/lab.env
mkdir -p "$SRC" "$DL" "$ART" "$IMGW" "$ETC" "$SECRETS" "$STATE" "$RUNL" "$LOG" "$LAB/tools"
chmod 0700 "$SECRETS" "$STATE" "$ETC"
```

## 3. Пакеты, Go, исходники, сборка

### 3.1 Пакеты (root)

Список выведен из кода: `ip`, `iptables-nft(+restore)`, `ip6tables-nft(+restore)`, `conntrack` (`hostinstall/preflight.go: requiredTools`),
`debugfs` (e2fsprogs; backend вызывает его при `personal_ssh`), `fakeroot`+`unsquashfs`+`mkfs.ext4` (подготовка образа),
`ssh` (шлюз `/usr/bin/ssh` жёстко прописан), `cp` (backend копирует образы через `cp --reflink=auto --sparse=always`).

```bash
# [sudo]
set -euo pipefail
sudo apt-get update
sudo apt-get install -y --no-install-recommends ca-certificates curl git make jq openssl \
  e2fsprogs squashfs-tools fakeroot iproute2 iptables conntrack openssh-client coreutils tar
for t in ip iptables-nft iptables-nft-restore ip6tables-nft ip6tables-nft-restore conntrack debugfs fakeroot unsquashfs mkfs.ext4 ssh ssh-keygen jq make git; do
  p=$(PATH=/usr/sbin:/usr/bin:/sbin:/bin command -v "$t") || { echo "MISSING $t"; exit 1; }; echo "$t -> $p"
done
```

Не включайте и не меняйте UFW/Docker/firewall вручную.

### 3.2 Своп (рекомендуется при 1 ГБ RAM; root)

Линковка `vmapi` (в зависимостях `modernc.org/sqlite`) может не поместиться в 1 ГБ. Потребление не измерялось.

```bash
# [sudo]  пропустите, если `swapon --show` уже показывает своп >= 1G
set -euo pipefail
if [ -z "$(swapon --show --noheadings)" ]; then
  sudo fallocate -l 2G /var/lib/vmlab-swap && sudo chmod 600 /var/lib/vmlab-swap
  sudo mkswap /var/lib/vmlab-swap && sudo swapon /var/lib/vmlab-swap
fi
swapon --show; free -m
```
(Файл подкачки удаляется при сбросе выделенного хоста оператором.)

### 3.3 Go 1.25.0 (без root, в `~/vmlab/tools/go`)

`go.mod`: `go 1.25.0`. `GOTOOLCHAIN=local` запрещает скрытые догрузки тулчейнов.

SHA-256 и размер Go 1.25.0 независимо сверены с официальным JSON
`https://go.dev/dl/?mode=json&include=all`: размер `59659213` байт.
Скачанный архив всё равно обязательно проверяется командой ниже; расхождение — **стоп**.

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env
GO_SHA256=2852af0cb20a13139b3448992e69b868e50ed0f8a1e5940ee1de9e19a123b613   # СВЕРИТЬ с https://go.dev/dl/
d=$(mktemp -d "$DL/go.XXXXXX")                       # новый пустой каталог для скачанного
curl -fL --proto '=https' --tlsv1.2 --max-filesize 300000000 -o "$d/go.tgz" https://go.dev/dl/go1.25.0.linux-amd64.tar.gz
echo "$GO_SHA256  $d/go.tgz" | sha256sum -c -
rm -rf "$GOROOT"; mkdir -p "$GOROOT"
tar -xzf "$d/go.tgz" -C "$GOROOT" --strip-components=1
go version                                           # ожидается: go version go1.25.0 linux/amd64
```

### 3.4 Исходники на закреплённом коммите и сборка

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env
rm -rf "$SRC"
git clone https://github.com/petruekhin/vibe-firecracker.git "$SRC"
git -C "$SRC" checkout --detach 92797338416f5bd84341d8d08e2724cb76b1ae47
test "$(git -C "$SRC" rev-parse HEAD)" = 92797338416f5bd84341d8d08e2724cb76b1ae47 && echo commit-ok
git -C "$SRC" status --short                         # должно быть пусто
cd "$SRC"
time make build                                      # статические (CGO_ENABLED=0) бинарники в $SRC/bin; GOMAXPROCS/GOFLAGS из lab.env
ls -l "$SRC/bin"
```

`make build` собирает все 8 исполняемых (`vmapi vmbot vmctl vm-launcher vm-network vm-helper vm-helper-client vmhost`);
`vmhost install` требует **все 8** в `--build-dir`, `vm-launcher` проверяется на отсутствие `PT_INTERP` (иначе отказ).
Если процесс убит OOM (`dmesg`/`Killed`) — убедитесь в §3.2 и повторите `make build` (кэш сохранится).

## 4. Проверка коллизий гостевого префикса 10.222

Слот 0 занимает `10.222.0.0/30` (хост `.1`, гость `.2`). Нужно, чтобы никакой маршрут/адрес не пересекал его
(в том числе покрывающий, например `10.0.0.0/8` на `eth0` — это **коллизия**, см. `vmnet.CheckCollisions`).

```bash
# [user] только чтение
set -euo pipefail
source ~/vmlab/lab.env
ip -4 route show table all
ip -4 -o addr show
```

Оцените вывод: нет ни одного маршрута/адреса, чья сеть пересекается с `10.222.0.0/30`
(пересечение — любая запись `10.0.0.0/8`, `10.222.0.0/16`, `10.222.0.0/24`, `10.0.0.0/7` и т.п. на **другом** устройстве, чем `tap-vm0`).
При пересечении выберите другой `GPFX` (например `10.223`), повторите проверку и измените `GPFX` в `lab.env` **до** §7;
иначе — стоп. Окончательная проверка — `vmhost preflight` (§8).

## 5. Загрузка артефактов с проверкой SHA-256

Значения взяты из `e2e/testdata/artifacts.json` (поле `bytes` там — верхняя граница загрузки, не точный размер).
Каждый файл скачивается в **новый пустой каталог**, хэш проверяется до использования.

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env
dl() { # url sha256 maxbytes outfile
  curl -fL --proto '=https' --tlsv1.2 --max-filesize "$3" -o "$4" "$1"
  echo "$2  $4" | sha256sum -c -
}
rm -rf "$DL/fc" "$DL/kernel" "$DL/rootfs"; mkdir "$DL/fc" "$DL/kernel" "$DL/rootfs"
dl https://github.com/firecracker-microvm/firecracker/releases/download/v1.12.1/firecracker-v1.12.1-x86_64.tgz \
   0a75e67ef6e4c540a2cf248b06822b0be9820cbba9fe19f9e0321200fe76ff6b 67108864 "$DL/fc/firecracker.tgz"
dl https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.12/x86_64/vmlinux-6.1.128 \
   27a8310b9a727517e9eb02044524b6ceb77de5728e3491b6974d5c846227ecc8 268435456 "$DL/kernel/vmlinux-6.1.128"
dl https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.12/x86_64/ubuntu-24.04.squashfs \
   88821a26b5a38c92b84a064d452167d7f80f9e17cf4441d1ebbae7569e340aee 2147483648 "$DL/rootfs/ubuntu-24.04.squashfs"
ls -l "$DL"/fc "$DL"/kernel "$DL"/rootfs

# извлечь ровно один член архива в чистый каталог
rm -rf "$DL/fcx"; mkdir "$DL/fcx"
tar -xzf "$DL/fc/firecracker.tgz" -C "$DL/fcx" release-v1.12.1-x86_64/firecracker-v1.12.1-x86_64
rm -rf "$ART"; mkdir -p "$ART"
install -m 0755 "$DL/fcx/release-v1.12.1-x86_64/firecracker-v1.12.1-x86_64" "$ART/firecracker"
install -m 0644 "$DL/kernel/vmlinux-6.1.128" "$ART/vmlinux"
"$ART/firecracker" --version | head -1               # ожидается v1.12.1
```

Несовпадение хэша — стоп (не подменяйте артефакты).

## 6. Базовый загрузочный ext4 с корректным владельцем root

Это переносит логику `e2e/fixture_image_test.go: PrepareImage` (в `_test.go`, публичной CLI нет — отдельного инструмента
не изобретаем, используем стандартные утилиты). Монтирования нет. Владельцы `root:root` сохраняются через общую базу
`fakeroot` (иначе файлы принадлежали бы `ubuntu`, и sshd отверг бы `/root`). Единственное изменение гостя — приведение
upstream-юнита `fcnet.service` к точному виду, который принимает продукт (добавляется `ExecStartPre=…udevadm trigger --settle…`).
`-O ^orphan_file` — гостевое ядро 6.1 не знает `orphan_file`.

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env
rm -rf "$IMGW"; mkdir -p "$IMGW"; cd "$IMGW"
fakeroot -s owners.db -- unsquashfs -processors 1 -no-progress -d tree "$DL/rootfs/ubuntu-24.04.squashfs"

# ровно ожидаемый upstream-юнит -> ровно поддерживаемый продуктом юнит
unit=tree/etc/systemd/system/fcnet.service
old=$'[Service]\nType=oneshot\nExecStart=/usr/local/bin/fcnet-setup.sh\n[Install]\nWantedBy=sshd.service'
[ "$(cat "$unit")" = "$old" ] || { echo "unexpected upstream fcnet unit; STOP"; exit 1; }
printf '%s\n' '[Service]' 'Type=oneshot' \
  'ExecStartPre=/usr/bin/udevadm trigger --settle --subsystem-match=net --action=add' \
  'ExecStart=/usr/local/bin/fcnet-setup.sh' '[Install]' 'WantedBy=sshd.service' > "$unit"

fakeroot -i owners.db -- mkfs.ext4 -q -F -O ^orphan_file -d tree rootfs.ext4 1024M
rm -rf tree
ls -ls rootfs.ext4                                    # 1 GiB видимый размер, разреженный
e2fsck -fn rootfs.ext4; echo "e2fsck exit=$?"         # ожидается 0
```

Проверка образа по контракту провижинеров (только чтение, `debugfs` без `-w`):

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env
cd "$IMGW"
dbg() { debugfs -R "$1" rootfs.ext4 2>/dev/null; }
dbg "stat /root"       | grep -E 'Type: directory.*|User:' | head -3        # User: 0 Group: 0
dbg "stat /etc/passwd" | grep -E 'User:'                                     # User: 0 Group: 0
dbg "cat /usr/lib/os-release" | grep -E '^ID=ubuntu$|^ID="ubuntu"$'
dbg "cat /etc/passwd" | grep -E '^root:x:0:0:[^:]*:/root:'
dbg "stat /etc/systemd/system/sysinit.target.wants/fcnet.service" | grep -E 'Fast link dest: "(/etc/systemd/system/fcnet.service|\.\./fcnet.service)"'
# содержимое сравнивается так же, как в продукте (хвостовые \n не значимы; $(...) их отбрасывает)
[ "$(dbg 'cat /etc/systemd/system/fcnet.service')" = "$(cat "$SRC/internal/sshaccess/supported/fcnet.service")" ] && echo fcnet-unit-ok
[ "$(dbg 'cat /usr/local/bin/fcnet-setup.sh')"     = "$(cat "$SRC/internal/sshaccess/supported/fcnet-setup.sh")" ]  && echo fcnet-script-ok
# debugfs сообщает об отсутствии в stderr; dbg() намеренно скрывает stderr, поэтому здесь прямой вызов:
missing=$(debugfs -R 'stat /etc/vibe-fc-ssh-access' rootfs.ext4 2>&1)
printf '%s\n' "$missing" | grep -Fq '/etc/vibe-fc-ssh-access: File not found by ext2_lookup'
echo access-marker-absent-ok
dbg "cat /etc/ssh/ssh_host_ed25519_key.pub" | awk '{print $1}'               # ssh-ed25519
dbg "ls -p /etc/systemd/system/fcnet.service.d" 2>&1 | head                  # каталог должен быть пуст/отсутствовать
```

Каждая проверка должна пройти; иначе — стоп (Create позже упадёт с неинформативным `operation not confirmed`).
Проверка отсутствия маркера выводит `access-marker-absent-ok`. Она проверяет именно ответ debugfs об отсутствии файла, а не произвольную ошибку чтения образа.

## 7. Отдельный loop-смонтированный том хранилища (root)

Разреженный файл + ext4 + loop-mount в путь, который ожидают юниты и `vmhost`. Это файл, а не блочное устройство —
физические диски не затрагиваются. Размер 10 GiB видимый, фактически расходуется по мере записи. Не добавляйте в
`/etc/fstab` (сброс окружения выполняет оператор; после перезагрузки монтирование нужно повторить).

```bash
# [sudo]
set -euo pipefail
source ~/vmlab/lab.env
test ! -e "$STORAGE_IMG" || { echo "storage image already exists; STOP"; exit 1; }
sudo install -d -m 0755 /var/lib/vmlab "$STORAGE"
sudo truncate -s 10G "$STORAGE_IMG"
sudo mkfs.ext4 -q -F -L vmlab-storage "$STORAGE_IMG"
sudo mount -o loop,nodev "$STORAGE_IMG" "$STORAGE"
findmnt "$STORAGE"
# доказательство «другое устройство», чем родитель (то же условие проверяют backend, юнит и preflight):
test "$(stat -c %d "$STORAGE")" != "$(stat -c %d "$STORAGE/..")" && echo mountpoint-ok
df -h "$STORAGE"
```

## 8. Установка продукта и хост-подготовка (строгий порядок)

Порядок важен: `setup` читает tmpfiles-файл, созданный `install`, и требует смонтированного хранилища;
`install` ничего не запускает и не создаёт pool.conf/каталоги/образ; `enable` только активирует сокет.

### 8.1 `vmhost install` (root) — бинарники, юниты, tmpfiles, ассеты

`--group ubuntu` — основная группа пользователя vmapi (группа сокета/lock/каталогов). Ассеты ставятся в
`/opt/firecracker-builder/{firecracker,vmlinux}`.

```bash
# [sudo]
set -euo pipefail
source ~/vmlab/lab.env
sudo "$SRC/bin/vmhost" install --build-dir "$SRC/bin" --group ubuntu \
  --firecracker "$ART/firecracker" --kernel "$ART/vmlinux"
ls -l /usr/local/bin/vmapi /usr/local/libexec/fc-vm-* /opt/firecracker-builder/
systemctl cat firecracker-vm@.service | grep -E 'MemoryMax|RootDirectory|Group='    # MemoryMax=768M, Group=ubuntu
```

### 8.2 `pool.conf` (root:root 0644), sysctl, модуль tun

Формат строго `KEY=VALUE`, без пробелов/кавычек/встроенных комментариев. Не копируйте пример (`10.213`, `MAX_SLOTS=2`).

```bash
# [sudo]
set -euo pipefail
source ~/vmlab/lab.env
printf 'GUEST_PREFIX=%s\nMAX_SLOTS=1\n' "$GPFX" > "$LAB/pool.conf.tmp"
cat "$LAB/pool.conf.tmp"
sudo install -D -m 0644 -o root -g root "$LAB/pool.conf.tmp" /etc/firecracker-vms/pool.conf
rm "$LAB/pool.conf.tmp"
stat -c '%U:%G %a %n' /etc/firecracker-vms /etc/firecracker-vms/pool.conf    # root:root 755 / root:root 644

# IPv4 forwarding: требование реализации (NAT/форвардинг гость<->хост через tap-vm0), не публикация
sudo sysctl -w net.ipv4.ip_forward=1
printf 'net.ipv4.ip_forward=1\n' | sudo tee /etc/sysctl.d/99-vmlab-forward.conf >/dev/null
sudo modprobe tun
test -c /dev/net/tun && echo tun-ok
```

### 8.3 `vmhost setup --apply` (root): каталоги jail, runtime-дерево, work/images

Создаёт `/etc/firecracker-vms`, `/run/firecracker-vms-net`, jail `/var/lib/firecracker-vms/jail/*`, и на смонтированном томе
`vms/` (root 0755), `vms/work` и `vms/images` (**`ubuntu:ubuntu` 0770**, по основной группе указанного пользователя);
затем `systemd-tmpfiles --create` (root-владелец `/run/firecracker-vms` 0755 и lock-файл `.backend.lock` root:ubuntu 0660).
Существующее не изменяется. Без `--user` отказ.

```bash
# [sudo]
set -euo pipefail
source ~/vmlab/lab.env
sudo "$SRC/bin/vmhost" setup --apply --user ubuntu
stat -c '%U:%G %a %n' "$STORAGE/vms" "$STORAGE/vms/work" "$STORAGE/vms/images" \
  /run/firecracker-vms /run/firecracker-vms/.backend.lock /var/lib/firecracker-vms/jail
# ожидается: root:root 755 / ubuntu:ubuntu 770 / ubuntu:ubuntu 770 / root:root 755 / root:ubuntu 660 / root:root 755
```

### 8.4 Предварительная проверка и активация сокета хелпера

```bash
# [user] preflight только читает; FAIL => стоп
set -euo pipefail
source ~/vmlab/lab.env
"$SRC/bin/vmhost" preflight --group ubuntu
```
Ожидаются `ok` для kvm, tun, systemd, tools, ip_forward, group, pool, subnet, storage, firecracker, kernel.
`warn` по `storage-space` допустим только если свободно < 5 GiB. Любой `FAIL` — стоп.

```bash
# [sudo] daemon-reload + enable --now fc-vm-helper.socket (ничего больше)
set -euo pipefail
source ~/vmlab/lab.env
sudo "$SRC/bin/vmhost" enable --apply --group ubuntu
systemctl is-active fc-vm-helper.socket               # active
stat -c '%U:%G %a %n' /run/fc-vm-helper.sock           # root:ubuntu 660
```
VM-юниты и сеть здесь **не** запускаются: хелпер сам поднимает слот при Create.

## 9. Базовый образ, ключи, токен, конфигурация

### 9.1 Публикация базового образа в каталог

Копия на том хранилища **под `ubuntu`**, обычный файл, затем read-only; `.tmp` не листится как образ.

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env
test ! -e "$STORAGE/vms/images/$IMAGE_ID.ext4"
cp --sparse=always "$IMGW/rootfs.ext4" "$STORAGE/vms/images/$IMAGE_ID.ext4.tmp"
chmod 0444 "$STORAGE/vms/images/$IMAGE_ID.ext4.tmp"
mv "$STORAGE/vms/images/$IMAGE_ID.ext4.tmp" "$STORAGE/vms/images/$IMAGE_ID.ext4"
stat -c '%U:%G %a %s %F %n' "$STORAGE/vms/images/$IMAGE_ID.ext4"    # ubuntu:ubuntu 444 1073741824 regular file
ls -A "$STORAGE/vms/images"                                          # НЕ должно быть каталога .committed (иначе образ уже не «базовый»)
```
(`.backend.lock` в этом каталоге создаст сам vmapi.)

### 9.2 Секреты (значения не печатать)

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env
umask 077
# Bearer-токен (файл 0600, один токен без пробелов — условие Credential.Resolve)
openssl rand -hex 32 > "$SECRETS/api-token"
# файл заголовка для curl -H @file: токен не попадает в argv/ps
{ printf 'Authorization: Bearer '; cat "$SECRETS/api-token"; } > "$SECRETS/api-header"
# мастер-ключ SSH-хранилища: ровно 32 случайных байта, 0600, владелец vmapi
head -c 32 /dev/urandom > "$SECRETS/ssh-master.key"
# наш собственный ключ для входа владельца в гостя (передаётся в Create как ssh_public_key)
ssh-keygen -q -t ed25519 -N '' -C labowner -f "$SECRETS/guest_ed25519"
stat -c '%a %s %n' "$SECRETS"/*       # токен 600, header 600, master 600 размер 32, ключи 600/644
```

### 9.3 known_hosts для кооперативного Stop и для нашего ssh

Ключ хоста гостя берётся **офлайн** из проверенного образа (не сканированием сети). Без этого файла Stop шлёт только
Ctrl-Alt-Del (`personal_ssh.shutdown_known_hosts_file` включает ещё и `ssh … systemctl reboot`), а idle-expiry был бы выключен.
Образ один и тот же для всех производных, ключ хоста наследуется.

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env
umask 077
pub=$(debugfs -R 'cat /etc/ssh/ssh_host_ed25519_key.pub' "$IMGW/rootfs.ext4" 2>/dev/null | awk '{print $1" "$2}')
case "$pub" in "ssh-ed25519 "*) ;; *) echo "no ed25519 host key; STOP"; exit 1;; esac
printf '%s.0.2 %s\n' "$GPFX" "$pub" > "$ETC/known_hosts"
chmod 0600 "$ETC/known_hosts"; ssh-keygen -l -f "$ETC/known_hosts"
```

### 9.4 Конфигурация vmapi

Отличия от примера: `terminal` отсутствует; `bastion` отсутствует; `legacy_owner_user_id` не нужен (пустое состояние);
`max_vms=1`; свой профиль; `personal_ssh` включён (даёт `provision-public-key`, т.е. приём `ssh_public_key`);
`guest_network_provisioning=ubuntu-fcnet`. Пути абсолютные, без симлинков. Файл 0600, лимит размера 128 KiB.
Строгий разбор: неизвестные поля — ошибка.

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env
umask 077
cat > "$ETC/vmapi.json" <<EOF
{
  "listen_addr": "127.0.0.1:8090",
  "state_dir": "$STATE",
  "allowed_user_ids": [$USER_ID],
  "shared_image_ids": ["$IMAGE_ID"],
  "max_vms": 1,
  "max_vms_per_user": 1,
  "default_profile": "$PROFILE",
  "default_ttl_seconds": 3600,
  "max_ttl_seconds": 86400,
  "idle_timeout_seconds": 3600,
  "credentials": [ { "token_file": "$SECRETS/api-token", "user_id": $USER_ID } ],
  "backend": {
    "work_root": "$STORAGE/vms/work",
    "images_dir": "$STORAGE/vms/images",
    "run_dir": "/run/firecracker-vms",
    "storage_mount": "$STORAGE",
    "max_vms": 1,
    "helper": "/usr/local/libexec/fc-vm-helper-client",
    "pool_conf": "/etc/firecracker-vms/pool.conf",
    "guest_prefix": "$GPFX",
    "guest_dns": ["1.1.1.1"],
    "guest_network_provisioning": "ubuntu-fcnet",
    "ssh_user": "root",
    "min_free_mib": 512,
    "max_total_mem_mib": $MEM_MIB,
    "max_total_vcpu": 1,
    "unit_mem_limit_mib": 768,
    "vmm_overhead_mib": 64,
    "default_profile": "$PROFILE",
    "profiles": [ { "name": "$PROFILE", "vcpu": 1, "mem_mib": $MEM_MIB, "disk_mib": 2048 } ],
    "personal_ssh": {
      "enabled": true,
      "store_dir": "$LAB/ssh-store",
      "master_key_file": "$SECRETS/ssh-master.key",
      "shutdown_known_hosts_file": "$ETC/known_hosts"
    }
  }
}
EOF
jq -e . "$ETC/vmapi.json" >/dev/null && echo json-ok
grep -c terminal "$ETC/vmapi.json" || true       # 0
stat -c '%a %n' "$ETC/vmapi.json"                # 600
```
Согласованность, которую проверяет код: `guest_prefix`/`max_vms` ≤ pool.conf (`GUEST_PREFIX=10.222`, `MAX_SLOTS=1`) — иначе vmapi
не стартует; профиль ≤ `max_total_*`; `256+64 ≤ 768`; CPU-лимит профиля (1000 мc) ≤ потолка пула (по умолчанию 2000).

## 10. Запуск vmapi

Нужен `PATH` с `/usr/sbin` (там `debugfs`; vmapi ищет его через PATH и иначе откажет в старте). Права файлов — umask 077.
vmapi держит эксклюзивные flock на work/run/images; запускайте **один** экземпляр.

**Вариант A (оболочка A, на переднем плане; отдельно не тестировался):**
```bash
# [user] оболочка A
source ~/vmlab/lab.env
cd "$LAB" && umask 077
exec env PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin /usr/local/bin/vmapi -config "$ETC/vmapi.json"
```
(Остановка — Ctrl-C.)

**Вариант B (одна оболочка, фон; проверен):**
```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env
umask 077
nohup setsid env PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin /usr/local/bin/vmapi -config "$ETC/vmapi.json" \
  >"$LOG/vmapi.log" 2>&1 < /dev/null &
echo $! > "$RUNL/vmapi.pid"
sleep 3; kill -0 "$(cat "$RUNL/vmapi.pid")" && echo running; tail -n 20 "$LOG/vmapi.log"
```
Сообщений-ошибок vmapi в stderr намеренно мало (тексты скрыты). При немедленном выходе проверьте по порядку: lock
`/run/firecracker-vms/.backend.lock` (root:ubuntu 0660), владельцев `work`/`images`, `findmnt $STORAGE`, `pool.conf`, мастер-ключ (0600, 32 байта),
наличие `debugfs` в PATH, права `~/vmlab` (не group/world-writable).

## 11. Вспомогательные функции для API (оболочка B)

```bash
# [user] сохраните как ~/vmlab/api.sh, затем в каждом блоке: source ~/vmlab/lab.env; source ~/vmlab/api.sh
cat > ~/vmlab/api.sh <<'EOF'
LAST="$RUNL/last.json"
# call METHOD PATH [IDEMPOTENCY_KEY] [JSON_BODY]  -> тело в $LAST, код HTTP в stderr; return 0 только при 2xx
call() {
  local m=$1 p=$2 k=${3:-} b=${4:-} args=() code
  args=(-sS --max-time 25 -H @"$SECRETS/api-header" -o "$LAST" -w '%{http_code}' -X "$m")
  [ -n "$k" ] && args+=(-H "Idempotency-Key: $k")
  [ -n "$b" ] && args+=(-H 'Content-Type: application/json' --data "$b")
  code=$(curl "${args[@]}" "$BASE$p") || { echo "curl failed" >&2; return 1; }
  echo "HTTP $code $m $p" >&2
  case "$code" in 2??) return 0;; *) cat "$LAST" >&2; echo >&2; return 1;; esac
}
# wait_op OPERATION_ID [DEADLINE_SECONDS]: 0=succeeded; 1=failed/HTTP; 2=slot unit failed; 3=deadline
wait_op() {
  local id=$1 end=$(( $(date +%s) + ${2:-240} )) st
  while [ "$(date +%s)" -lt "$end" ]; do
    call GET "/v1/operations/$id" || return 1
    st=$(jq -r .status "$LAST")
    case "$st" in
      succeeded) return 0;;
      failed) echo "operation failed:" >&2; cat "$LAST" >&2; echo >&2; return 1;;
    esac
    if systemctl is-failed --quiet firecracker-vm@0.service; then echo "firecracker-vm@0 is in failed state" >&2; return 2; fi
    sleep 2
  done
  echo "deadline exceeded waiting for $id" >&2; return 3
}
# wait_gone PATH [DEADLINE]: ждёт 404
wait_gone() {
  local end=$(( $(date +%s) + ${2:-120} )) code
  while [ "$(date +%s)" -lt "$end" ]; do
    code=$(curl -sS --max-time 25 -H @"$SECRETS/api-header" -o /dev/null -w '%{http_code}' "$BASE$1")
    [ "$code" = 404 ] && return 0; sleep 2
  done; echo "still visible: $1" >&2; return 3
}
diag() { sudo journalctl --no-pager -n 80 -u 'firecracker-vm@0' -u 'firecracker-vm-network@0' -u 'fc-vm-helper@*' -u fc-vm-helper.socket; }
EOF
```

Ключи идемпотентности: 1–128 печатных ASCII без пробелов; уникальные на операцию. Используем `RUNID=$(date +%s)` и суффиксы.

Все вложенные команды SSH используют `-n`: иначе при запуске bash-блока через stdin SSH может поглотить оставшуюся часть самого скрипта.

## 12. Сценарий: Create → poll → SSH → Stop → Commit → Delete

Переменные между блоками не сохраняются — состояние записывается в `$RUNL/*.id`.

### 12.1 Здоровье, аутентификация, capabilities

```bash
# [user] оболочка B
set -euo pipefail
source ~/vmlab/lab.env; source ~/vmlab/api.sh
curl -sS "$BASE/healthz"; echo                                  # {"status":"ok"}  ("degraded" => стоп)
curl -sS -o /dev/null -w '%{http_code}\n' "$BASE/v1/vms"         # 401 без токена
call GET /v1/capabilities && jq . "$LAST"
# ожидается: max_vms=1, profiles=[{name:lab256,vcpu:1,memory_mib:256}], ssh_public_key_supported=true, ttl.default_seconds=3600
call GET /v1/images && jq . "$LAST"                              # содержит lab-base (shared=true, phase=ready)
call GET /v1/vms && jq . "$LAST"                                 # {"vms":[]}
systemctl is-failed firecracker-vm@0.service || true             # "inactive" (не "failed"); если failed: sudo systemctl reset-failed firecracker-vm@0.service
```

### 12.2 Create (с идемпотентным повтором)

Тело: только `name`, `image_id`, `profile`, `ttl_seconds`, `ssh_public_key` (канонический ключ без требований к комментарию).
`ttl_seconds` 1800 ≤ 86400.

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env; source ~/vmlab/api.sh
RUNID=$(date +%s); echo "$RUNID" > "$RUNL/runid"
PUB=$(cat "$SECRETS/guest_ed25519.pub")
BODY=$(jq -cn --arg k "$PUB" --arg img "$IMAGE_ID" --arg p "$PROFILE" \
  '{name:"labvm", image_id:$img, profile:$p, ttl_seconds:1800, ssh_public_key:$k}')
call POST /v1/vms "lab-$RUNID-create" "$BODY"                    # ожидается HTTP 202
jq -r .vm_id "$LAST"        > "$RUNL/vm.id"
jq -r .operation_id "$LAST" > "$RUNL/create.op"
# идемпотентный повтор того же ключа и того же тела: те же operation_id/vm_id
call POST /v1/vms "lab-$RUNID-create" "$BODY"
[ "$(jq -r .vm_id "$LAST")" = "$(cat "$RUNL/vm.id")" ] && [ "$(jq -r .operation_id "$LAST")" = "$(cat "$RUNL/create.op")" ] && echo replay-ok
```
Тело с `resources`, неизвестным полем или другим телом при том же ключе → 422/409 (так и должно быть). Не повторяйте Create с новым ключом,
если первый прошёл: квота — 1 VM.

### 12.3 Poll до завершения Create (дедлайн 240 с, контроль failed-состояния)

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env; source ~/vmlab/api.sh
if ! wait_op "$(cat "$RUNL/create.op")" 240; then echo "CREATE FAILED -> диагностика"; diag; call GET "/v1/vms/$(cat "$RUNL/vm.id")" || true; jq . "$LAST"; exit 1; fi
call GET "/v1/vms/$(cat "$RUNL/vm.id")" && jq '{id,name,state,phase,profile,resources,expires_at}' "$LAST"   # phase=active, state=running
systemctl is-active firecracker-vm@0.service firecracker-vm-network@0.service    # active active
ip -br addr show tap-vm0                                                          # UP, 10.222.0.1/30
sudo iptables-nft -S FCVM0F | head -5                                             # правила цепочки слота (внутренняя изоляция/NAT)
```

### 12.4 SSH-метаданные и аутентифицированный SSH

`GET /v1/vms/{id}/ssh` не ждёт загрузки: `ready:false` — норма, пока гость грузится (один TCP-проб к `10.222.0.2:22`).
Опрос с дедлайном 150 с:

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env; source ~/vmlab/api.sh
id=$(cat "$RUNL/vm.id"); end=$(( $(date +%s) + 150 )); ready=false
while [ "$(date +%s)" -lt "$end" ]; do
  call GET "/v1/vms/$id/ssh"; ready=$(jq -r .ready "$LAST"); [ "$ready" = true ] && break
  systemctl is-failed --quiet firecracker-vm@0.service && { echo "slot unit failed"; diag; exit 2; }
  sleep 3
done
[ "$ready" = true ] || { echo "ssh never ready (deadline)"; diag; exit 3; }
jq '{host,port,user,ready,auth_mode}' "$LAST"        # host=10.222.0.2 port=22 user=root auth_mode=provision-public-key
H=$(jq -r .host "$LAST"); P=$(jq -r .port "$LAST"); U=$(jq -r .user "$LAST")
[ "$H" = "$GPFX.0.2" ] && [ "$P" = 22 ] && [ "$U" = root ] || { echo "unexpected ssh metadata"; exit 1; }
# аутентифицированный вход с жёстким pinning (known_hosts из §9.3), с повтором до дедлайна 90 с
end=$(( $(date +%s) + 90 )); ok=0
while [ "$(date +%s)" -lt "$end" ]; do
  out=$(ssh -n -F /dev/null -i "$SECRETS/guest_ed25519" -p "$P" -o IdentityAgent=none -o IdentitiesOnly=yes \
    -o BatchMode=yes -o PasswordAuthentication=no -o PreferredAuthentications=publickey \
    -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$ETC/known_hosts" -o GlobalKnownHostsFile=/dev/null \
    -o ConnectTimeout=8 -o LogLevel=ERROR "$U@$H" 'echo vmmanager-ok; id -un; free -m | sed -n 2p' 2>&1) && { ok=1; break; }
  sleep 3
done
[ "$ok" = 1 ] || { echo "ssh login failed: $out"; exit 4; }
echo "$out"
# маркер для проверки сохранности (используется после Commit только косвенно; см. §15)
ssh -n -F /dev/null -i "$SECRETS/guest_ed25519" -p "$P" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes \
  -o UserKnownHostsFile="$ETC/known_hosts" -o GlobalKnownHostsFile=/dev/null -o LogLevel=ERROR "$U@$H" 'echo lab-marker > /root/.lab-marker && sync'
```
Если `Permission denied` — смотрите, что провижининг ключа прошёл (Create завершился успешно, значит `Install` сработал); если
`Host key verification failed` — известный ключ из §9.3 не совпал (не отключайте проверку: стоп).
Если гость не загружается при 256 MiB (в журнале/ничего не отвечает на 22): это запасной вариант — удалите VM (§13.x «аварийная уборка»), в `lab.env`
поставьте `MEM_MIB=512`, пересоздайте `vmapi.json` (§9.4; `max_total_mem_mib` станет 512, `512+64 ≤ 768`) и перезапустите vmapi. Иначе — стоп.

### 12.5 Stop (cold) и Commit

Commit требует корректного выхода VMM (receipt `success/exited/0`). Сам Commit тоже умеет останавливать VM; здесь отдельный Stop проверяет его API. После Stop SSH-метаданные дают `ready:false`.

На закреплённом коммите воспроизведён переходный сбой: VMM уже завершился чисто, но Stop-операция стала `failed/unavailable`, а VM осталась `stopping`. Это дефект подтверждения исхода в продукте, не исправленный этим руководством. Документированный повтор Stop с новым ключом через API восстанавливает `stopped` по receipt исходного VMM; ограниченный повтор был отдельно вручную проверен после наблюдавшегося терминального сбоя Stop, а в финальном чистом прогоне не понадобился. Следующий блок делает не более трёх попыток только после терминального `failed/unavailable`; при неизвестном исходе или ином сбое останавливается. Это не повод убивать VMM, удалять state или обходить receipt-проверку.
Имя образа — метка по тем же правилам (`lab-snap`).

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env; source ~/vmlab/api.sh
RUNID=$(cat "$RUNL/runid"); id=$(cat "$RUNL/vm.id")
# Эта версия иногда завершает VMM раньше подтверждения Stop в API.
# Повтор только ПОСЛЕ terminal failed/unavailable, с НОВЫМ ключом;
# тот же ключ вернул бы старую failed-операцию. Никаких kill/reset/ручных правок.
stop_ok=0
for attempt in 1 2 3; do
  call POST "/v1/vms/$id/stop" "lab-$RUNID-stop-$attempt" '{}'
  op=$(jq -r .operation_id "$LAST")
  if wait_op "$op" 180; then stop_ok=1; break; fi
  if ! jq -e '.status == "failed" and .error.code == "unavailable"' "$LAST" >/dev/null; then
    echo "Stop outcome not terminal failed/unavailable; STOP"; diag; exit 1
  fi
  echo "Stop not confirmed; retrying through API after terminal failure ($attempt/3)"
  sleep 2
done
[ "$stop_ok" = 1 ] || { echo "Stop still not confirmed after 3 attempts; STOP"; diag; exit 1; }
call GET "/v1/vms/$id"; jq '{state,phase}' "$LAST"              # phase=stopped
call GET "/v1/vms/$id/ssh"; jq -r .ready "$LAST"                # false
systemctl is-active firecracker-vm@0.service || true            # inactive (а не failed)
ls /run/firecracker-vms/slot-0/                                 # <invocation>.exit.json
sudo cat /run/firecracker-vms/slot-0/*.exit.json                # result=success exit_code=exited exit_status=0

call POST "/v1/vms/$id/commit" "lab-$RUNID-commit" '{"name":"lab-snap"}'
jq -r .image_id "$LAST" > "$RUNL/image.id"; op=$(jq -r .operation_id "$LAST")
wait_op "$op" 300 || { diag; exit 1; }
call GET "/v1/images/$(cat "$RUNL/image.id")" && jq . "$LAST"   # phase=ready, shared=true, can_delete=true
ls -l "$STORAGE/vms/images" "$STORAGE/vms/images/.committed"    # новый <id>.ext4 (0444) и его маркер
```
В текущей версии ready-образы входят в общий реестр: `shared=true` нормально и для committed-образа. `can_delete=true` означает право создателя удалить его; базовый `lab-base` защищён конфигурацией. Поэтому не сохраняйте в общем образе секреты, которые не должны быть доступны другим пользователям API. В этом локальном запуске пользователь один.

Commit — холодный (с остановленной VM): публикация делается из отдельной копии диска, ключи, установленные продуктом,
вычищаются (`ScrubVM`) только из копии; исходный диск и базовый образ не меняются.

## 13. Cleanup тестовых ресурсов (до остановки сервиса) и остановка

Порядок: Delete VM → ждать 404 → Delete производного образа → ждать 404 → убедиться, что юнит слота неактивен →
только потом остановка vmapi. Удаляется **только созданное здесь** (базовый образ `lab-base` — shared и не удаляется через API).

```bash
# [user]
set -euo pipefail
source ~/vmlab/lab.env; source ~/vmlab/api.sh
RUNID=$(cat "$RUNL/runid"); id=$(cat "$RUNL/vm.id")
call DELETE "/v1/vms/$id" "lab-$RUNID-delete-vm"; op=$(jq -r .operation_id "$LAST")
wait_op "$op" 180 || { diag; exit 1; }
wait_gone "/v1/vms/$id" 120
if [ -s "$RUNL/image.id" ]; then
  img=$(cat "$RUNL/image.id")
  call DELETE "/v1/images/$img" "lab-$RUNID-delete-img"; op=$(jq -r .operation_id "$LAST")
  wait_op "$op" 120 || exit 1
  wait_gone "/v1/images/$img" 60
fi
call GET /v1/vms  && jq -c . "$LAST"      # {"vms":[]}
call GET /v1/images && jq -c '[.images[].id]' "$LAST"           # ["lab-base"]
systemctl is-active firecracker-vm@0.service || true             # inactive
ls "$STORAGE/vms/work/vms/" ; ls -A "$STORAGE/vms/images" "$STORAGE/vms/images/.committed"   # рабочий каталог VM удалён; остался lab-base.ext4, .committed пуст
```

**Аварийная уборка** (если сценарий прервался): `call GET /v1/vms`, для каждого `id` — `DELETE /v1/vms/{id}` с новым ключом и `wait_op`,
затем `GET /v1/images` и удалить образы, кроме `lab-base`. Никогда не удаляйте файлы из `work/`/`images/` руками при живом vmapi
(retained-диск без receipt — уникальное состояние; см. `deployment/multivm/README.md`, «Operator procedure»).

Остановка сервиса (после успешной уборки):

```bash
# [user] вариант A: Ctrl-C в оболочке A; вариант B:
source ~/vmlab/lab.env
kill -TERM "$(cat "$RUNL/vmapi.pid")"; sleep 3; ! kill -0 "$(cat "$RUNL/vmapi.pid")" 2>/dev/null && echo vmapi-stopped
```
Остановка vmapi не останавливает VM и не удаляет данные. Сеть слота (`tap-vm0`, цепочки `FCVM0*`) остаётся, пока активен
`firecracker-vm-network@0` (`RemainAfterExit`). При необходимости, **только** когда `firecracker-vm@0` неактивен:
`sudo systemctl stop firecracker-vm-network@0.service` (убирает только помеченные собственные TAP/правила).
Полный сброс окружения (отключение сокета, `vmhost uninstall`, umount/удаление `storage.img`, своп, sysctl-файл,
`/etc/firecracker-vms`, `/opt/firecracker-builder`, `~/vmlab`) — выполняется оператором при сбросе выделенного хоста и здесь не описывается.

## 14. Расхождения старых документов с кодом (не доверять без проверки)

* `docs/control-api.md` описывает `resources` в Create и `ssh_public_key_supported:false`; в коде `resources` отвергается, а
  `ssh_public_key`/`ttl_seconds` и поле `profile` — принимаются (при `personal_ssh`). Источник: `validate.go: ParseCreate`, `cmd/vmapi/main.go`.
* Примеры конфигов содержат плейсхолдеры (`bastion.example.com`, `10.213`, `USER`, `terminal.listen_addr 0.0.0.0:8443`, `MAX_SLOTS=2`) — не копируются как есть.
* `deployment/multivm/*` упоминает «legacy firecracker-builder»; он не нужен, но путь `/var/lib/firecracker-builder/storage` — действующий умолчательный путь.
* `docs/install.md`: «Nothing is started» после `install` — верно; `host-enable` включает только `fc-vm-helper.socket`.

## 15. Ограничения и что НЕ проверено

* Команды проверены на чистой Ubuntu 24.04 x86_64 (вложенный KVM, 1 ГБ RAM, обычный пользователь `ubuntu` с passwordless sudo) на коммите `92797338416f5bd84341d8d08e2724cb76b1ae47`; другие дистрибутивы, пользователи и окружения не проверялись. При первой неописанной ошибке — стоп и отчёт.
* Проверен фоновый запуск (вариант B) и цепочка Create → SSH → Stop → cold Commit → Delete. Вариант A (на переднем плане) отдельно не тестировался.
* SHA-256 Go 1.25.0 (§3.3) сверен с официальным JSON go.dev; хэши firecracker/ядра/rootfs взяты из `e2e/testdata/artifacts.json`. Все загруженные файлы проверяются перед использованием.
* Пути по умолчанию содержат `firecracker-builder` (наследие совместимости); это ожидаемо, их не нужно менять.
* Загрузка закреплённого Ubuntu 24.04 CI-rootfs при 256 MiB RAM и вход по SSH проверены; запасной вариант §12.4 не потребовался.
* Пик памяти сборки (`GOGC/GOMEMLIMIT/-p=1/swap`) не измерялся; `GOMEMLIMIT` — мягкий предел.
* Подготовка образа (`mkfs.ext4 -O ^orphan_file -d` под fakeroot) отработала на пакетах чистой Ubuntu 24.04; на других версиях не проверялась.
* Не проверены: производный VM из committed-образа, TTL-истечение, перезапуск vmapi, idle-expiry (включён через `shutdown_known_hosts_file`, окно 3600 с; SSH-сессия должна быть открыта, иначе VM удаляется по таймауту — тест укладывается в минуты).
* Идентичность ключа хоста: один и тот же ключ у всех VM из одного образа (в `known_hosts` выставлен только слот 0).
* Loop-том не переживает перезагрузку (нет `fstab`); после ребута нужно смонтировать том до старта слота (юнит `AssertPathIsMountPoint` иначе откажет).
* Переходный Stop `failed/unavailable` (§12.5) **не исправлен**. Наблюдение: VMM завершился чисто, а Stop-операция стала `failed/unavailable`; повтор Stop с новым idempotency key вручную восстановил `stopped`. Первопричина не доказана и требует отдельного разбора в продукте.
* Единственный слот: во время работы второй VM создать нельзя (`429 capacity`/`conflict`); Commit работает на остановленной VM, удерживающей слот.

## 16. Ссылки на источники (коммит 9279733)

* Установщик/подготовка хоста: `cmd/vmhost/main.go`, `internal/hostinstall/{plan,install,setup,preflight,options}.go`, `Makefile`, `docs/install.md`.
* Юниты и пул: `deployment/multivm/{firecracker-vm@.service,firecracker-vm-network@.service,fc-vm-helper@.service,fc-vm-helper.socket,firecracker-vms.slice,firecracker-vms.tmpfiles.conf,pool.conf.example,NETWORK.md}`.
* Хелпер/пул/CPU-квота: `internal/vmhelper/vmhelper.go`, `cmd/vm-helper/main.go`, `cmd/vm-helper-client/main.go`; лаунчер/receipt: `cmd/vm-launcher/main.go`.
* Сеть слота (TAP/NAT/коллизии/`need exactly one default route`): `internal/vmnet/{vmnet,cli}.go`.
* Backend: `vmlib/local/backend.go` (checkStorage, Prepare, Start, Publish, Release, boot args), `vmlib/local/trust.go` (дерево `/run`), `vmlib/local/images.go`, `vmlib/local/real.go`.
* Конфиги: `internal/localcfg/{config,access,access_linux,access_paths_linux,shutdown_linux,build_linux}.go`, `internal/controlapi/{config,validate,http}.go`, `cmd/vmapi/main.go`.
* Образ/SSH/сеть гостя: `internal/sshaccess/{provision,network,network_contract,store,shutdown}.go`, `internal/sshaccess/supported/*`, `docs/ssh-access.md`.
* Источники артефактов и подготовка образа: `e2e/testdata/artifacts.json`, `e2e/fixture_artifacts_test.go`, `e2e/fixture_image_test.go`, `e2e/disposable_test.go`; последовательность API: `e2e/live_test.go` (`TestLifecycleStopCommitsAndConflicts`).
* SSH-метаданные: `vmlib/service.go: SSHInfo`, `internal/vmbot/control.go: ControlSSH`.
