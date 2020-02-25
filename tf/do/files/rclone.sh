#!/usr/bin/bash

while :; do
   if command -v rclone; then
      break
   else
      echo waiting for rclone ...
      sleep 10
      # need to discuss with LC
      break
   fi
done

# wait for harmony.service to start
sleep 30

# stop harmony service
echo stopping harmony.service
sudo systemctl stop harmony.service

unset shard

# determine the shard number
for s in 3 2 1; do
   if [ -d harmony_db_${s} ]; then
      shard=${s}
      # download shard db
      echo rclone syncing harmony_db_${shard}
      rclone sync -P mainnet:pub.harmony.one/mainnet/harmony_db_${shard} /root/do-user/harmony_db_${shard}
      break
   fi
done

# download beacon chain db anyway
echo rclone syncing harmony_db_0
rclone sync -P mainnet:pub.harmony.one/mainnet/harmony_db_0 /root/do-user/harmony_db_0

# restart the harmony service
echo restarting harmony.service
sudo systemctl start harmony.service