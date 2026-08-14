/# check if root=... was set/i\
# Boot the root filesystem carried inside this initramfs, if asked to. The\
# helper execs switch_root and never returns when it applies.\
if [ -f /etc/laserbridge/ramboot-init ]; then\
\t. /etc/laserbridge/ramboot-init\
fi\
# Locate the selected A/B partition next to the uniquely labelled ESP.\
case "$KOPT_root" in\
\tLASERBRIDGE_ROOT_A|LASERBRIDGE_ROOT_B)\
\t\tnlplug-findfs -p /sbin/mdev LABEL=LBBOOT\
\t\tlaserbridge_bootdev=$(findfs LABEL=LBBOOT)\
\t\tcase "$KOPT_root" in LASERBRIDGE_ROOT_A) laserbridge_part=2 ;; *) laserbridge_part=3 ;; esac\
\t\tcase "$laserbridge_bootdev" in\
\t\t\t*1) KOPT_root="${laserbridge_bootdev%1}${laserbridge_part}" ;;\
\t\t\t*) echo "Could not locate LaserBridgeOS root beside LBBOOT" ;;\
\t\tesac\
\t\t;;\
esac
