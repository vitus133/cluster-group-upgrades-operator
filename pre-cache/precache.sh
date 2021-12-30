#!/bin/bash
cwd="${cwd:-/opt/precache}"
. $cwd/common
copy_environment
[[ $? -eq 0 ]] || exit 1
/opt/precache/release 
/opt/precache/olm 
/opt/precache/pull