rm -f /etc/sysctl.conf /etc/sysctl.d/99-sysctl.conf

# Measurement shim: the container cannot write /proc/sys (rootless; every
# "sysctl -w" fails with permission denied). The shim keeps written values in
# /tmp/sysctl.state, answers "-n" from it, logs calls to /tmp/sysctl.calls and
# passes everything else (-e, -a) to the real binary. kernel.hostname is made to
# fail like the real one.
mv /usr/sbin/sysctl /usr/sbin/sysctl.real
cat >/usr/sbin/sysctl <<'SHIMEOF'
#!/bin/sh
echo "sysctl $*" >>/tmp/sysctl.calls
case "$1" in
  -w) shift
      for kv; do
        k=${kv%%=*}; v=${kv#*=}
        if [ "$k" = kernel.hostname ]; then echo "sysctl: permission denied on key \"$k\"" >&2; exit 1; fi
        { grep -v "^$k=" /tmp/sysctl.state 2>/dev/null; echo "$k=$v"; } >/tmp/sysctl.new; mv /tmp/sysctl.new /tmp/sysctl.state
        echo "$k = $v"
      done ;;
  -n) v=$(grep "^$2=" /tmp/sysctl.state 2>/dev/null | tail -1 | cut -d= -f2-)
      if [ -n "$v" ]; then echo "$v"; else exec /usr/sbin/sysctl.real -n "$2"; fi ;;
  *) exec /usr/sbin/sysctl.real "$@" ;;
esac
SHIMEOF
chmod +x /usr/sbin/sysctl

