#!/bin/sh
export DATASTORE_TYPE=kubernetes

# The same parameters for felix should be passed to typha, or some of the dataplane configuration
# will be overwrited.

# default for veth
export FELIX_LOGSEVERITYSYS=none
export FELIX_LOGSEVERITYSCREEN=info
export CALICO_NETWORKING_BACKEND=none
export CLUSTER_TYPE=k8s,aliyun
export CALICO_DISABLE_FILE_LOGGING=true
# shellcheck disable=SC2154
export FELIX_IPTABLESREFRESHINTERVAL="${IPTABLESREFRESHINTERVAL:-60}"
export FELIX_IPV6SUPPORT=true
export WAIT_FOR_DATASTORE=true
export NO_DEFAULT_POOLS=true
export FELIX_DEFAULTENDPOINTTOHOSTACTION=ACCEPT
export FELIX_HEALTHENABLED=true
export FELIX_LOGFILEPATH=/dev/null
export FELIX_BPFENABLED=false
export FELIX_XDPENABLED=false
export FELIX_BPFCONNECTTIMELOADBALANCINGENABLED=false
export FELIX_BPFKUBEPROXYIPTABLESCLEANUPENABLED=false

export FELIX_ALLOWVXLANPACKETSFROMWORKLOADS=true
export FELIX_ALLOWIPIPPACKETSFROMWORKLOADS=true
export FELIX_INTERFACEPREFIX="hybr"

export FELIX_IPTABLESMANGLEALLOWACTION=RETURN

exec 2>&1

if [ -n "$NODENAME" ]; then
    export FELIX_FELIXHOSTNAME="$NODENAME"
fi

if [ -n "$DATASTORE_TYPE" ]; then
    export FELIX_DATASTORETYPE="$DATASTORE_TYPE"
fi

if [ "$(cat /sys/module/ipv6/parameters/disable)" -ne "0" ] || [ "$(cat /proc/sys/net/ipv6/conf/all/disable_ipv6)" -ne "0" ] ; then
    export FELIX_IPV6SUPPORT=false
fi

exec /hybridnet/"${1}"
