vmmanager: локальный пример с настоящим Firecracker

Нужны чистая Ubuntu 24.04 x86_64, systemd, доступ к /dev/kvm и около 12 ГБ
свободного места. Распакуйте архив в /opt, чтобы пользователь vmapi мог
обращаться к бинарникам, а не в закрытый домашний каталог:

   sudo tar -xzf vmmanager-example.tar.gz -C /opt
   sudo -i
   cd /opt/vmmanager-example

Дальше команды выполняются от root из папки этого примера.
Бинарники, Firecracker, ядро гостя, образ Ubuntu и конфиги уже в архиве.
Интернет нужен только для установки системных пакетов.

1. Пакеты и пользователь для запуска API

   apt-get update
   apt-get install -y iproute2 iptables conntrack e2fsprogs openssh-client openssl
   useradd --system --user-group --no-create-home --home-dir /var/lib/vmmanager/api --shell /usr/sbin/nologin vmapi

2. Хранилище: разреженный файл на 10 ГБ, монтирование для текущего сеанса

   mkdir -p /var/lib/vmmanager /var/lib/firecracker-builder/storage
   truncate -s 10G /var/lib/vmmanager/storage.img
   mkfs.ext4 -q -F /var/lib/vmmanager/storage.img
   mount -o loop,nodev /var/lib/vmmanager/storage.img /var/lib/firecracker-builder/storage

3. Хостовые компоненты, сеть и привилегированный helper

   vmhost — утилита подготовки хоста, не сервер: install размещает бинарники
   и внутренние systemd-юниты; setup создаёт рабочие каталоги и назначает права;
   preflight проверяет готовность, ничего не исправляя. Helper принимает
   ограниченные привилегированные запросы на запуск VM от непривилегированной API.

   ./bin/vmhost install --group vmapi --firecracker assets/firecracker --kernel assets/vmlinux
   install -D -m 0644 config/pool.conf /etc/firecracker-vms/pool.conf
   sysctl -w net.ipv4.ip_forward=1
   modprobe tun
   ./bin/vmhost setup --apply --user vmapi
   ./bin/vmhost preflight --group vmapi
   systemctl daemon-reload
   systemctl start fc-vm-helper.socket

4. Готовый базовый образ, конфиг и ключи этой установки

   cp --sparse=always images/ubuntu.ext4 /var/lib/firecracker-builder/storage/vms/images/ubuntu.ext4
   chown vmapi:vmapi /var/lib/firecracker-builder/storage/vms/images/ubuntu.ext4
   chmod 0444 /var/lib/firecracker-builder/storage/vms/images/ubuntu.ext4
   install -d -m 0700 -o vmapi -g vmapi /var/lib/vmmanager/api /var/lib/vmmanager/api/state /var/lib/vmmanager/api/ssh-store
   install -m 0600 -o vmapi -g vmapi config/vmapi.json config/known_hosts /var/lib/vmmanager/api/
   umask 077
   openssl rand -hex 32 > /var/lib/vmmanager/api/api-token
   head -c 32 /dev/urandom > /var/lib/vmmanager/api/ssh-master.key
   chown vmapi:vmapi /var/lib/vmmanager/api/api-token /var/lib/vmmanager/api/ssh-master.key

5. Запуск API в терминале; Ctrl+C останавливает API

   runuser -u vmapi -- ./bin/vmapi -config /var/lib/vmmanager/api/vmapi.json

   Swagger: http://127.0.0.1:8090/docs
   В Authorize вставьте содержимое /var/lib/vmmanager/api/api-token.
   Базовый образ для создания VM: ubuntu.

Для повторного запуска API в текущем сеансе нужен только шаг 5.
После перезагрузки повторите монтирование из шага 2, команды sysctl и modprobe
из шага 3, затем systemctl start fc-vm-helper.socket и шаг 5.
Не повторяйте шаги 1–4 поверх установленного примера: они создают хранилище
и новые секреты. API не устанавливается как служба и не запускается автоматически.
Остановка API не останавливает VM: внутренние VMM, сеть и helper используют systemd.

Troubleshooting
 - Нет /dev/kvm: включите аппаратную или вложенную виртуализацию.
 - FAIL в preflight: исправьте указанную причину до запуска API.
 - Пересечение подсетей: по умолчанию используется 10.222.0.0/30. Выберите другой
   префикс, например 10.223, в /etc/firecracker-vms/pool.conf и guest_prefix в
   /var/lib/vmmanager/api/vmapi.json; замените 10.222.0.2 на 10.223.0.2 в
   /var/lib/vmmanager/api/known_hosts. Конфиги из шага 4 должны быть уже скопированы.
 - API сразу завершается: проверьте монтирование командой
   findmnt /var/lib/firecracker-builder/storage и права файлов из шага 4.
 - Мало места: файл хранилища по мере заполнения может занять до 10 ГБ.
