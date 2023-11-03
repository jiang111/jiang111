echo "====================================================================="
echo "Start to install jdk 11"
echo "====================================================================="
 
while true; do
    echo "y" | apt-get install software-properties-common
    if [ $? -eq 0 ]; then
        echo "命令执行成功。"
        break
    else
        echo "命令执行失败。继续尝试。"
    fi
done

add-apt-repository ppa:openjdk-r/ppa

while true; do
    echo "y" | apt-get update
    if [ $? -eq 0 ]; then
        echo "命令执行成功。"
        break
    else
        echo "命令执行失败。继续尝试。"
    fi
done
while true; do
    echo "y" | apt install -y openjdk-11-jdk
    if [ $? -eq 0 ]; then
        echo "命令执行成功。"
        break
    else
        echo "命令执行失败。继续尝试。"
    fi
done
java -version

echo "====================================================================="
echo "Start to install android sdk"
echo "====================================================================="
 
wget https://dl.google.com/android/android-sdk_r24.4.1-linux.tgz

tar -zxf  android-sdk_r24.4.1-linux.tgz


export ANDROID_HOME="/root/workspace/NewFeiFanApp_7iSy/android-sdk-linux"
if ! grep "ANDROID_HOME=/root/workspace/NewFeiFanApp_7iSy/android-sdk-linux" /etc/profile 
then
echo "ANDROID_HOME=/root/workspace/NewFeiFanApp_7iSy/android-sdk-linux" | sudo tee -a /etc/profile
echo "export ANDROID_HOME" | sudo tee -a /etc/profile

echo "PATH=${ANDROID_HOME}/tools:${ANDROID_HOME}/platform-tools:$PATH" | sudo tee -a /etc/profile
echo "export PATH" | sudo tee -a /etc/profile
fi

source /etc/profile  
export PATH="/root/workspace/NewFeiFanApp_7iSy/android-sdk-linux/tools:$PATH"
export PATH="/root/workspace/NewFeiFanApp_7iSy/android-sdk-linux/platform-tools:$PATH"


function install_sdk {
  android update sdk -u -s -a -t "$1"
}

function fetch_non_obsoled_package_indices {
  # Fetch the sdk list using non-https connections
  android list sdk -u -s -a |\
    # Filter obsoleted packages
    sed '/\(Obsolete\)/d' |\
    # Filter to take only the index number of package
    sed 's/^[ ]*\([0-9]*\).*/\1/' |\
    # Remove the empty lines
    sed -n 's/^[^ $]/\0/p'
}

for package_index in  $(fetch_non_obsoled_package_indices)
do
  echo "====================================================================="
  echo "Start to install package:  ${package_index}"
  echo "====================================================================="
  # Auto accept license
  echo -e "y" | install_sdk "${package_index}"
  echo
  echo
done


export ANDROID_SDK_ROOT="/root/workspace/NewFeiFanApp_7iSy/android-sdk-linux"

echo "====================================================================="
echo "Start to install flutter sdk"
echo "====================================================================="
 
git clone https://github.com/flutter/flutter.git -b stable
cd ./flutter/bin
chmod 774 flutter
chmod 774 dart
./flutter doctor
export PATH=PATH=$PATH:$(pwd)
cd ..
echo flutter.sdk=$(pwd) > emas_config.local.properties
echo sdk.dir="/root/workspace/NewFeiFanApp_7iSy/android-sdk-linux" > emas_config.local.properties
cat emas_config.local.properties > ../android/local.properties
cd ..
echo "====================================================================="
echo "Start to 构建一套环境:"
echo "====================================================================="
 
flutter build apk --release --target ./lib/main_dev.dart
echo "构建完成"
echo 'Android 包文件路径:'
echo $(pwd)/build/app/outputs/flutter-apk/app-release.apk