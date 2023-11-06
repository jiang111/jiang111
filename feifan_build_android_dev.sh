echo "====================================================================="
echo "Start to install jdk 11"
echo "====================================================================="
 

mkdir java11
cd java11
wget https://raw.githubusercontent.com/jiang111/jiang111/master/jdk-11/Archive.zip
wget https://raw.githubusercontent.com/jiang111/jiang111/master/jdk-11/Archive2.zip
wget https://raw.githubusercontent.com/jiang111/jiang111/master/jdk-11/Archive3.zip

unzip Archive.zip
unzip Archive2.zip
unzip Archive3.zip

cd ..

export JAVA_HOME=/root/workspace/NewFeiFanApp_7iSy/java11
export JRE_HOME=${JAVA_HOME}/jre
export CLASSPATH=.:${JAVA_HOME}/lib:${JRE_HOME}/lib
export PATH=${JAVA_HOME}/bin:$PATH
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


while true; do
    echo "y" | android update sdk -u -s -a -t platform-tool,android-34,sysimg-34,build-tools-34.0.0
    # 检查命令的退出状态
    if [ $? -eq 0 ]; then
        echo "命令执行成功。"
        break
    else
        echo "命令执行失败。继续尝试。"
    fi
done

echo "y" | /root/workspace/NewFeiFanApp_7iSy/android-sdk-linux/tools/bin/sdkmanager --install "cmdline-tools;latest"
yes | /root/workspace/NewFeiFanApp_7iSy/android-sdk-linux/tools/bin/sdkmanager --licenses


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