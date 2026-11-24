mkdir -p /mnt/x
printf "tmpfs\t/mnt/x\ttmpfs\tdefaults\t0\t0\n" > /etc/fstab
mount -t tmpfs tmpfs /mnt/x
(cd /mnt/x; nohup sleep 600 >/dev/null 2>&1 &)
