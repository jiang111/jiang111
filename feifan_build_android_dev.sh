apt install -y openjdk-8-jdk
java -version

wget https://dl.google.com/android/android-sdk_r24.4.1-linux.tgz

tar -zxf  android-sdk_r24.4.1-linux.tgz

export ANDROID_SDK_ROOT="/root/workspace/NewFeiFanApp_7iSy/android-sdk-linux"

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

while true; do
    echo "y" | android update sdk --force --no-ui --all --filter platform-tools,android-33
    if [ $? -eq 0 ]; then
        echo "命令执行成功。"
        break
    else
        echo "命令执行失败。继续尝试。"
    fi
done


git clone https://github.com/flutter/flutter.git -b stable
cd ./flutter/bin
chmod 774 flutter
chmod 774 dart
./flutter doctor
export PATH=PATH=$PATH:$(pwd)
cd ..
echo flutter.sdk=$(pwd) > emas_config.local.properties
cat emas_config.local.properties > ../android/local.properties
cd ..
echo "开始构建1套环境的 Android 包:"
flutter build apk --release --target ./lib/main_dev.dart
echo "构建完成"
echo 'Android 包文件路径:'
echo $(pwd)/build/app/outputs/flutter-apk/app-release.apk