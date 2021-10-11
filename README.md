IPFS-Drive-rclone是IPFS Drive的命令行工具，登录 www.ipfsdrive.com 下载软件和查阅详情命令行工具和配置文档



IPFS Drive是一个去中心化云盘/存储应用,帮助用户连接到Web3.stroage和Filswan等IPFS存储服务商，实现云盘/存储功能。

# step1：克隆https://github.com/IPFSDrive/IPFS-drive 项目到本地

# step2：需要构建命令行 
克隆https://github.com/IPFSDrive/IPFS-Drive-rclone 项目
在项目目录执行：

go get

go build -tags cmount

将产生的rclone.exe复制到ipfsdrive项目extra-resource发布版本对应目录，如:win64/下
  
# step3:需要构建Webui
克隆https://github.com/IPFSDrive/rclone-ui 项目
在项目目录执行：

npm install

npm build

将产生的build文件夹复制到ipfsdrive项目extra-resource对应目录,如:win64/下
	
# Step4:构建ipfsdrive
在项目ipfsdrive目录执行：

npm run package_win32_

npm run start-electron

最终在dist文件夹产生ipfsdrive运行文件包


登录 www.ipfsdrive.com 下载软件和查阅详情命令行工具和配置文档
