

echo "====================================================================="
echo "Start to install java 17 sdk"
echo "====================================================================="
 
# 更新java 版本,老版本无法编译
wget -q https://download.oracle.com/java/17/latest/jdk-17_linux-x64_bin.tar.gz
tar -xvf jdk-17_linux-x64_bin.tar.gz
export JAVA_HOME=`pwd`/jdk-17.0.9
export JRE_HOME=${JAVA_HOME}/jre
export CLASSPATH=.:${JAVA_HOME}/lib:${JRE_HOME}/lib
export PATH=${JAVA_HOME}/bin:$PATH
java --version


echo "====================================================================="
echo "Start to download Android commandlinetools"
echo "====================================================================="


wget -q https://dl.google.com/android/repository/commandlinetools-linux-10406996_latest.zip

unzip commandlinetools-linux-10406996_latest.zip

mv cmdline-tools latest
mkdir sdk
mkdir sdk/cmdline-tools
mv latest sdk/cmdline-tools

export ANDROID_HOME=`pwd`/sdk

export PATH=$ANDROID_HOME/cmdline-tools/latest/bin:$PATH

echo "y" | sdkmanager "platform-tools" "system-images;android-34;default;arm64-v8a" "build-tools;34.0.0"

#echo "y" | sdkmanager --install "cmdline-tools;latest"

echo "y" | sdkmanager  --licenses

export PATH="$ANDROID_HOME/tools:$PATH"
export PATH="$ANDROID_HOME/platform-tools:$PATH"
export ANDROID_SDK_ROOT="$ANDROID_HOME"


# 重新配jdk 保证环境是新版的
export JAVA_HOME=`pwd`/jdk-17.0.9
export JRE_HOME=${JAVA_HOME}/jre
export CLASSPATH=.:${JAVA_HOME}/lib:${JRE_HOME}/lib
export PATH=${JAVA_HOME}/bin:$PATH
java --version


echo "====================================================================="
echo "Start to install flutter sdk"
echo "====================================================================="
 
git clone https://github.com/flutter/flutter.git -b 3.13.9
export PATH="$PATH:`pwd`/flutter/bin"
echo flutter.sdk="$(pwd)/flutter" > emas_config.local.properties
echo sdk.dir="$(pwd)/sdk" > emas_config.local.properties
cat emas_config.local.properties > ./android/local.properties

flutter config --android-sdk `pwd`/sdk

echo "y" | flutter doctor

echo "====================================================================="
echo "Start to 构建一套环境:"
echo "====================================================================="
 
flutter build apk --release --target ./lib/main_dev.dart
echo "构建完成"
echo 'Android 包文件路径:'
echo $(pwd)/build/app/outputs/flutter-apk/app-release.apk
