git config --global http.sslVerify false
git config --global http.postBuffer 1048576000
git config --global https.postBuffer 1048576000


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

flutter_version=$(curl -s https://raw.githubusercontent.com/jiang111/jiang111/master/flutter.version)
git clone -b $flutter_version https://github.com/flutter/flutter.git --depth 1
cd flutter
git fetch --unshallow
cd ..
export PATH="$PATH:`pwd`/flutter/bin"
echo flutter.sdk="$(pwd)/flutter" > emas_config.local.properties
echo sdk.dir="$(pwd)/sdk" > emas_config.local.properties
cat emas_config.local.properties > ./android/local.properties

flutter config --android-sdk `pwd`/sdk

echo "y" | flutter doctor

echo "====================================================================="
echo "Start to 构建二套环境:"
echo "====================================================================="

flutter pub get

MAX_RETRIES=2
BUILD_COMMAND="flutter build apk --release --target ./lib/main_dev2.dart"
attempt=1
while [ $attempt -le $MAX_RETRIES ]; do
    echo "开始第 $attempt 次构建尝试"
    $BUILD_COMMAND

    # 检查构建是否成功
    if [ $? -eq 0 ]; then
        echo "Build successful."
        echo "构建完成"
        echo 'Android 包文件路径:'
        echo $(pwd)/build/app/outputs/flutter-apk/app-release.apk
        echo "y" | apt install qrencode
        qrencode -o $(pwd)/build/app/outputs/flutter-apk/qrcode.png -s 6 "http://mapptest2.feifan.art/apk/app-release.apk"
        exit 0
    else
        echo "Build failed."
        attempt=$((attempt + 1))
    fi
    sleep 1
done
exit 1
