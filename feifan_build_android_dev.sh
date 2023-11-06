

echo "====================================================================="
echo "Start to install android sdk"
echo "====================================================================="
 
echo "y" |  apt update
while true; do
    echo "y" | apt install android-sdk
    # 检查命令的退出状态
    if [ $? -eq 0 ]; then
        echo "命令执行成功。"
        break
    else
        echo "命令执行失败。继续尝试。"
    fi
done

export ANDROID_HOME="/usr/lib/android-sdk"
if ! grep "ANDROID_HOME=/usr/lib/android-sdk" /etc/profile 
then
echo "ANDROID_HOME=/usr/lib/android-sdk" | sudo tee -a /etc/profile
echo "export ANDROID_HOME" | sudo tee -a /etc/profile

echo "PATH=${ANDROID_HOME}/tools:${ANDROID_HOME}/platform-tools:$PATH" | sudo tee -a /etc/profile
echo "export PATH" | sudo tee -a /etc/profile
fi

source /etc/profile  
export PATH="/usr/lib/android-sdk/tools:$PATH"
export PATH="/usr/lib/android-sdk/platform-tools:$PATH"


cd /usr/lib/android-sdk

wget https://dl.google.com/android/repository/commandlinetools-linux-6609375_latest.zip


unzip commandlinetools-linux-6609375_latest.zip -d cmdline-tools

export PATH=$ANDROID_HOME/cmdline-tools/tools/bin:$PATH

yes | sdkmanager  --licenses

echo "y" | sdkmanager --update


echo "y" | sdkmanager "platform-tools" "build-tools;34.0.0"
yes | sdkmanager  --licenses

export ANDROID_SDK_ROOT="/usr/lib/android-sdk"


cd /root/workspace/NewFeiFanApp_7iSy
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
echo sdk.dir="/usr/lib/android-sdk" > emas_config.local.properties
cat emas_config.local.properties > ../android/local.properties
cd ..



while true; do
    echo "y" | flutter doctor --android-licenses
    # 检查命令的退出状态
    if [ $? -eq 0 ]; then
        echo "命令执行成功。"
        break
    else
        echo "命令执行失败。继续尝试。"
    fi
done



echo "====================================================================="
echo "Start to 构建一套环境:"
echo "====================================================================="
 
flutter build apk --release --target ./lib/main_dev.dart
echo "构建完成"
echo 'Android 包文件路径:'
echo $(pwd)/build/app/outputs/flutter-apk/app-release.apk